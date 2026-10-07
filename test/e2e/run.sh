#!/usr/bin/env bash
# End-to-end suite: real backups from real database containers, restored by
# the real lazarus binary, reported to a real control plane, dispatched to a
# real worker. Unit tests can't catch the class of bugs this covers (client
# output quirks, signing against real S3/Azure/GCS servers, RBAC wiring).
#
# Requires: docker, go, sqlite3, gpg, age, curl, python3.
# Optional: E2E_SKIP_MONGO=1 skips MongoDB (its image is ~700MB).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/lazarus-e2e.XXXXXX")"
PREFIX="lz-e2e-$$"
CP_PORT="${E2E_CP_PORT:-18090}"
CP="http://127.0.0.1:${CP_PORT}"
PIDS=()

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mE2E FAILED: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  docker ps -aq --filter "name=${PREFIX}" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker ps -aq --filter "label=lazarus.sandbox=true" | xargs -r docker rm -f >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

for bin in docker go sqlite3 gpg age curl python3; do
  command -v "$bin" >/dev/null || fail "missing prerequisite: $bin"
done

wait_for() { # wait_for <seconds> <command...>
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -lt $deadline ] || return 1
    sleep 1
  done
}

json_get() { # json_get <url> <python expr over `d`> [token]
  curl -fsS -H "Authorization: Bearer ${3:-adm-e2e}" "$1" | python3 -c "import json,sys; d=json.load(sys.stdin); print($2)"
}

log "Building binaries"
(cd "$ROOT" && go build -o "$WORK/lazarus" ./cmd/lazarus && go build -o "$WORK/lazarus-server" ./cmd/server)
BK="$WORK/bk"; mkdir -p "$BK"

log "Creating real backups from source databases"
docker run -d --name "${PREFIX}-pg" -e POSTGRES_PASSWORD=p -e POSTGRES_USER=app -e POSTGRES_DB=shop postgres:16-alpine >/dev/null
docker run -d --name "${PREFIX}-my" -e MYSQL_ROOT_PASSWORD=p -e MYSQL_DATABASE=shop mysql:8 >/dev/null
docker run -d --name "${PREFIX}-rd" redis:7-alpine >/dev/null
[ "${E2E_SKIP_MONGO:-}" = 1 ] || docker run -d --name "${PREFIX}-mg" -e MONGO_INITDB_ROOT_USERNAME=r -e MONGO_INITDB_ROOT_PASSWORD=p mongo:7.0 >/dev/null

wait_for 60 docker exec "${PREFIX}-pg" psql -U app -d shop -c 'select 1' || fail "source postgres not ready"
docker exec "${PREFIX}-pg" psql -q -U app -d shop -c "CREATE TABLE users(id serial primary key, name text); INSERT INTO users(name) SELECT 'u'||g FROM generate_series(1,500) g; CREATE TABLE orders(id serial primary key, created_at timestamptz default now()); INSERT INTO orders DEFAULT VALUES;"
docker exec "${PREFIX}-pg" pg_dump -U app shop | gzip > "$BK/pg.sql.gz"
docker exec "${PREFIX}-pg" pg_dump -U app -Fc shop > "$BK/pg.dump"
docker exec "${PREFIX}-pg" pg_dump -U app --schema-only shop > "$BK/pg-schemaonly.sql"
echo "INSERT INTO users(name) VALUES ('patched');" > "$BK/pg-patch-001.sql"

docker exec "${PREFIX}-rd" sh -c 'redis-cli SET k1 v1 && redis-cli SET k2 v2 && redis-cli SAVE' >/dev/null
docker cp "${PREFIX}-rd:/data/dump.rdb" "$BK/redis.rdb" >/dev/null

wait_for 120 docker exec "${PREFIX}-my" mysql -uroot -pp -e 'select 1' || fail "source mysql not ready"
docker exec "${PREFIX}-my" mysql -uroot -pp shop -e "CREATE TABLE items(id int primary key auto_increment, sku varchar(20)); INSERT INTO items(sku) VALUES ('a'),('b'),('c');" 2>/dev/null
docker exec "${PREFIX}-my" mysqldump -uroot -pp shop 2>/dev/null > "$BK/my.sql"

if [ "${E2E_SKIP_MONGO:-}" != 1 ]; then
  wait_for 90 docker exec "${PREFIX}-mg" mongosh -u r -p p --quiet --eval 'db.adminCommand("ping")' || fail "source mongo not ready"
  docker exec "${PREFIX}-mg" mongosh -u r -p p --quiet --eval 'db.getSiblingDB("lazarus_verify").users.insertMany([{n:1},{n:2},{n:3}])' >/dev/null
  docker exec "${PREFIX}-mg" mongodump -u r -p p --authenticationDatabase admin --db lazarus_verify --archive 2>/dev/null > "$BK/mongo.archive"
fi

# Source databases have done their job; free the CPU/RAM for the sandboxes.
docker ps -aq --filter "name=${PREFIX}-" | xargs -r docker rm -f >/dev/null 2>&1 || true

sqlite3 "$BK/app.db" "CREATE TABLE tenants(id integer primary key, n text); INSERT INTO tenants(n) VALUES ('a'),('b');"
echo pass | gpg --batch --yes --pinentry-mode loopback --passphrase-fd 0 -c -o "$BK/app.db.gpg" "$BK/app.db" 2>/dev/null
age-keygen -o "$WORK/age.key" 2>/dev/null
age -r "$(grep -o 'age1[0-9a-z]*' "$WORK/age.key")" -o "$BK/pg.sql.gz.age" "$BK/pg.sql.gz"
mkdir -p "$WORK/chaos"; cp "$BK/app.db" "$WORK/chaos/c-old.db"; touch -t 202601010000 "$WORK/chaos/c-old.db"; cp "$BK/app.db" "$WORK/chaos/c-new.db"

log "Starting S3 (versitygw), GCS (fake-gcs-server) and Azure (Azurite) emulators"
mkdir -p "$WORK/s3data" "$WORK/gcsdata/lz-bucket/nested"
cp "$BK/pg.sql.gz" "$WORK/gcsdata/lz-bucket/nested/pg dump.sql.gz"
docker run -d --name "${PREFIX}-s3" -p 19100:7070 -e ROOT_ACCESS_KEY=lzaccess -e ROOT_SECRET_KEY=lzsecret123 -v "$WORK/s3data:/data" versity/versitygw posix --nometa /data >/dev/null
docker run -d --name "${PREFIX}-gcs" -p 14543:4443 -v "$WORK/gcsdata:/data" fsouza/fake-gcs-server -scheme http -public-host localhost:14543 >/dev/null
docker run -d --name "${PREFIX}-az" -p 10100:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0 --skipApiVersionCheck --loose >/dev/null
AZKEY="Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==" # Azurite's public dev key
wait_for 30 curl -fs "http://127.0.0.1:14543/storage/v1/b/lz-bucket/o" || fail "fake-gcs not ready"
wait_for 30 curl -s -o /dev/null "http://127.0.0.1:19100" || fail "versitygw not ready"
wait_for 30 curl -s -o /dev/null "http://127.0.0.1:10100" || fail "azurite not ready"
curl -fsS -o /dev/null -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" --user "lzaccess:lzsecret123" "http://127.0.0.1:19100/lz-bucket"
curl -fsS -o /dev/null -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" --user "lzaccess:lzsecret123" -H "x-amz-content-sha256: UNSIGNED-PAYLOAD" --data-binary "@$BK/my.sql" "http://127.0.0.1:19100/lz-bucket/dumps/my.sql"
(cd "$ROOT" && go run ./test/e2e/azupload "http://127.0.0.1:10100/devstoreaccount1" devstoreaccount1 "$AZKEY" lzc "nested/pg dump.sql.gz" "$BK/pg.sql.gz")

log "Starting control plane with RBAC and named users"
cat > "$WORK/users.yml" <<EOF
users:
  - {name: alice, role: admin, key: alice-e2e}
EOF
"$WORK/lazarus-server" -addr "127.0.0.1:${CP_PORT}" -admin-key adm-e2e -viewer-key view-e2e -users-file "$WORK/users.yml" \
  -state "$WORK/server.json" >"$WORK/server.log" 2>&1 &
PIDS+=($!)
wait_for 15 curl -fs "$CP/healthz" || fail "control plane did not start"

MONGO_TARGET=""
if [ "${E2E_SKIP_MONGO:-}" != 1 ]; then
  MONGO_TARGET="  - name: mongo
    engine: mongodb
    path: $BK/mongo.archive
    checks: [{name: users, sql: \"db.users.countDocuments()\", expect_equal: 3}]"
fi

cat > "$WORK/lazarus.yml" <<EOF
state_file: $WORK/state.json
parallelism: 4
notify: {format: lazarus, when: always, webhook_url: "$CP/api/v1/reports"}
targets:
  - name: pg-plain
    engine: postgres
    path: $BK/pg.sql.gz
    tags: [prod]
    max_age: 1h
    size_drift: {max_decrease_pct: 50}
    auto_schema_check: true
    schema_baseline: [users, orders]
    incremental_patches: ["$BK/pg-patch-*.sql"]
    pre_drill_command: "echo pre > $WORK/hook-pre"
    post_drill_command: "echo \$LAZARUS_STATUS > $WORK/hook-post"
    checks:
      - {name: users incl patch, sql: "SELECT count(*) FROM users", expect_equal: 501}
      - {name: orders fresh, sql: "SELECT max(created_at) FROM orders", max_rpo: 1h}
  - name: pg-custom
    engine: postgres
    path: $BK/pg.dump
    tags: [prod]
    checks: [{name: users, sql: "SELECT count(*) FROM users", expect_min: 500}]
  - name: pg-schema-only-trap
    engine: postgres
    path: $BK/pg-schemaonly.sql
    remediation: {command: "echo \$LAZARUS_STAGE > $WORK/remediation"}
    checks: [{name: users populated, sql: "SELECT count(*) FROM users", expect_min: 1}]
  - name: mysql
    engine: mysql
    path: $BK/my.sql
    checks:
      - {name: items, sql: "SELECT count(*) FROM items", expect_equal: 3}
      - {name: skus, sql: "SELECT GROUP_CONCAT(sku ORDER BY sku) FROM items", expect_string: "a,b,c"}
      - {name: fresh, sql: "SELECT UTC_TIMESTAMP()", max_rpo: 1h}
  - name: redis
    engine: redis
    path: $BK/redis.rdb
    checks: [{name: keys, sql: "DBSIZE", expect_equal: 2}]
  - name: sqlite-gpg
    engine: sqlite
    path: $BK/app.db.gpg
    checks: [{name: tenants, sql: "SELECT count(*) FROM tenants", expect_equal: 2}]
  - name: pg-age
    engine: postgres
    path: $BK/pg.sql.gz.age
    checks: [{name: users, sql: "SELECT count(*) FROM users", expect_equal: 500}]
  - name: sqlite-chaos
    engine: sqlite
    path: "$WORK/chaos/c-*.db"
    fallback_on_failure: true
    chaos: {enabled: true}
    checks: [{name: tenants, sql: "SELECT count(*) FROM tenants", expect_min: 1}]
  - name: s3-mysql
    engine: mysql
    path: $WORK/dl/s3/my.sql
    cleanup_backup: true
    s3: {bucket: lz-bucket, key: dumps/my.sql, endpoint: "http://127.0.0.1:19100", access_key_id: lzaccess, secret_access_key: lzsecret123}
    checks: [{name: items, sql: "SELECT count(*) FROM items", expect_equal: 3}]
  - name: gcs-pg
    engine: postgres
    path: $WORK/dl/gcs/pg.sql.gz
    gcs: {bucket: lz-bucket, object: "nested/pg dump.sql.gz", endpoint: "http://127.0.0.1:14543"}
    checks: [{name: users, sql: "SELECT count(*) FROM users", expect_equal: 500}]
  - name: azure-pg
    engine: postgres
    path: $WORK/dl/az/pg.sql.gz
    azure: {account_name: devstoreaccount1, container: lzc, blob: "nested/pg dump.sql.gz", endpoint: "http://127.0.0.1:10100/devstoreaccount1", account_key: "$AZKEY"}
    checks: [{name: users, sql: "SELECT count(*) FROM users", expect_equal: 500}]
$MONGO_TARGET
EOF

log "Running all drills"
set +e
LAZARUS_API_KEY=alice-e2e LAZARUS_GPG_PASSPHRASE=pass LAZARUS_AGE_KEY_FILE="$WORK/age.key" \
  "$WORK/lazarus" --config "$WORK/lazarus.yml" --json >"$WORK/results.json" 2>"$WORK/run.err"
code=$?
set -e
[ "$code" = 1 ] || { cat "$WORK/run.err"; fail "exit code $code, want 1 (exactly the trap target fails)"; }
python3 - "$WORK/results.json" <<'PY' || fail "drill verdicts"
import json, sys
res = {r["target"]: r for r in json.load(open(sys.argv[1]))}
bad = [n for n, r in res.items() if r["passed"] != (n != "pg-schema-only-trap")]
for n in bad:
    print(f"  unexpected verdict: {n} passed={res[n]['passed']} err={res[n].get('error')}")
assert not bad
assert res["sqlite-chaos"].get("fallback_used"), "chaos target should pass via fallback"
print(f"  {len(res)} targets, verdicts as expected")
PY
[ "$(cat "$WORK/hook-pre")" = pre ] && [ "$(cat "$WORK/hook-post")" = PASSED ] || fail "pre/post hooks"
[ "$(cat "$WORK/remediation")" = checks ] || fail "remediation playbook"
[ ! -e "$WORK/dl/s3/my.sql" ] || fail "cleanup_backup should remove the downloaded S3 file"

log "Control plane: RBAC, attribution and reported fields"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$CP/api/v1/targets")" = 401 ] || fail "anonymous read should be 401"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer view-e2e' -d '{}' "$CP/api/v1/reports")" = 403 ] || fail "viewer report should be 403"
[ "$(json_get "$CP/api/v1/targets" "[t for t in d if t['name']=='pg-plain'][0]['has_rpo_check']" view-e2e)" = True ] || fail "RPO fields not reported"
[ "$(json_get "$CP/api/v1/targets" "[t for t in d if t['name']=='pg-plain'][0]['recent_history'][-1]['reported_by']")" = alice ] || fail "report attribution"
curl -fsS -X POST -H "Authorization: Bearer alice-e2e" "$CP/api/v1/targets/redis/mute" >/dev/null
[ "$(json_get "$CP/api/v1/audit" "d[0]['actor']+':'+d[0]['action']")" = "alice:mute" ] || fail "audit log"

log "Worker: dispatch, ownership and lease requeue"
python3 - "$WORK/lazarus.yml" "$WORK/worker.yml" <<'PY'
import re, sys
s = open(sys.argv[1]).read()
head, body = s.split("targets:\n", 1)
blocks = re.split(r"(?m)^(?=  - name: )", body)
keep = [b for b in blocks if b.startswith("  - name: pg-custom")]
open(sys.argv[2], "w").write(head + "targets:\n" + "".join(keep))
PY
start_worker() {
  LAZARUS_API_KEY=adm-e2e "$WORK/lazarus" --config "$WORK/worker.yml" --worker --control-plane "$CP" \
    --worker-id e2e-worker --interval 1s --quiet --live=false >>"$WORK/worker.log" 2>&1 &
  WORKER_PID=$!
  PIDS+=($WORKER_PID)
}
start_worker
wait_for 15 sh -c "curl -fs -H 'Authorization: Bearer adm-e2e' $CP/api/v1/workers | grep -q e2e-worker" || fail "worker did not register"

curl -fsS -X POST -H "Authorization: Bearer adm-e2e" "$CP/api/v1/targets/mysql/trigger" >/dev/null
curl -fsS -X POST -H "Authorization: Bearer adm-e2e" "$CP/api/v1/targets/pg-custom/trigger" >/dev/null
claimed() { [ "$(json_get "$CP/api/v1/targets" "[t for t in d if t['name']=='pg-custom'][0].get('claimed_by','')")" = e2e-worker ]; }
wait_for 20 claimed || fail "worker did not claim pg-custom"

# Kill the worker mid-drill: the lease must be requeued once it's offline.
kill -9 "$WORKER_PID"
wait "$WORKER_PID" 2>/dev/null || true
docker ps -q --filter "label=lazarus.sandbox=true" | xargs -r docker rm -f >/dev/null 2>&1 || true
pending() { [ "$(json_get "$CP/api/v1/targets" "[t for t in d if t['name']=='pg-custom'][0].get('trigger_pending',False)")" = True ]; }
wait_for 120 pending || fail "lease was not requeued after worker died"
[ "$(json_get "$CP/api/v1/targets" "[t for t in d if t['name']=='mysql'][0].get('trigger_pending',False)")" = True ] || fail "trigger for a target the worker lacks was consumed"

start_worker
done_again() { [ "$(json_get "$CP/api/v1/targets" "str(not [t for t in d if t['name']=='pg-custom'][0].get('trigger_pending',False) and not [t for t in d if t['name']=='pg-custom'][0].get('claimed_by'))")" = True ]; }
wait_for 90 done_again || fail "requeued drill was not completed by the restarted worker"
[ "$(json_get "$CP/api/v1/audit?limit=500" "any(e['action']=='requeue' for e in d)")" = True ] || fail "requeue not audited"

log "Graceful shutdown with an open SSE stream"
curl -sN -H "Authorization: Bearer view-e2e" "$CP/api/v1/stream" >/dev/null &
PIDS+=($!)
sleep 1
SERVER_PID="${PIDS[0]}"
start=$SECONDS
kill -TERM "$SERVER_PID"
while kill -0 "$SERVER_PID" 2>/dev/null; do sleep 0.2; done
[ $((SECONDS - start)) -lt 4 ] || fail "shutdown blocked on SSE"

log "E2E PASSED"
