#!/usr/bin/env bash
# Lazarus 5-Second Zero-Dependency Quickstart Drill
# Demonstrates backup verification with SQLite (No Docker or cloud credentials required).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Check sqlite3 prerequisite
if ! command -v sqlite3 &> /dev/null; then
    echo "Error: 'sqlite3' CLI is required to generate mock backup and verify SQLite targets." >&2
    echo "Please install sqlite3 (e.g. brew install sqlite3, apt install sqlite3, or apk add sqlite3)." >&2
    exit 1
fi

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/lazarus-demo-XXXXXX")"
cleanup() {
    rm -rf "${TMP_DIR}"
    if [[ -n "${LAZARUS_BIN}" && "${LAZARUS_BIN}" == *"/lazarus-bin-"* ]]; then
        rm -f "${LAZARUS_BIN}"
    fi
}
trap cleanup EXIT INT TERM

# Find lazarus binary or build on the fly
LAZARUS_BIN=""
if [ -x "${ROOT_DIR}/lazarus" ]; then
    LAZARUS_BIN="${ROOT_DIR}/lazarus"
elif command -v lazarus &> /dev/null; then
    LAZARUS_BIN="$(command -v lazarus)"
elif command -v go &> /dev/null; then
    echo "==> Compiling temporary lazarus binary..."
    LAZARUS_BIN="$(mktemp "${TMPDIR:-/tmp}/lazarus-bin-XXXXXX")"
    go build -o "${LAZARUS_BIN}" "${ROOT_DIR}/cmd/lazarus"
else
    echo "Error: lazarus binary not found. Please run 'make build' first." >&2
    exit 1
fi

echo "=========================================================="
echo "⚡ Lazarus 5-Second Quickstart Demonstration"
echo "   Working directory: ${TMP_DIR}"
echo "=========================================================="

# 1. Create a sample SQLite database file simulating a backup archive
DB_PATH="${TMP_DIR}/production-sample.db"
sqlite3 "${DB_PATH}" <<'EOF'
CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, email TEXT, status TEXT);
INSERT INTO users (name, email, status) VALUES ('Alice', 'alice@example.com', 'active');
INSERT INTO users (name, email, status) VALUES ('Bob', 'bob@example.com', 'active');
INSERT INTO users (name, email, status) VALUES ('Charlie', 'charlie@example.com', 'pending');
EOF

# 2. Generate a minimal Lazarus configuration
CONFIG_PATH="${TMP_DIR}/lazarus.demo.yml"
cat > "${CONFIG_PATH}" <<EOF
state_file: ${TMP_DIR}/lazarus-state.json
parallelism: 1
targets:
  - name: quickstart-sqlite
    engine: sqlite
    path: ${DB_PATH}
    max_age: 1h
    checks:
      - name: users table is populated
        sql: SELECT count(*) FROM users
        expect_min: 1
      - name: at least 2 active users exist
        sql: SELECT count(*) FROM users WHERE status = 'active'
        expect_min: 2
EOF

echo "==> Running Lazarus verification against simulated backup..."
echo ""

"${LAZARUS_BIN}" --config "${CONFIG_PATH}"

echo ""
echo "=========================================================="
echo "✨ Quickstart drill completed successfully!"
echo "   Temporary files will now be cleaned up automatically."
echo "=========================================================="
