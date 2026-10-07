# Lazarus

[![GitHub Release](https://img.shields.io/github/v/release/qscgy5713/Lazarus?color=blue&style=flat-square)](https://github.com/qscgy5713/Lazarus/releases)
[![CI Status](https://img.shields.io/github/actions/workflow/status/qscgy5713/Lazarus/ci.yml?branch=main&label=CI&style=flat-square)](https://github.com/qscgy5713/Lazarus/actions/workflows/ci.yml)
[![Docker GHCR](https://img.shields.io/badge/docker-ghcr.io-blue?logo=docker&style=flat-square)](https://github.com/qscgy5713/Lazarus/pkgs/container/lazarus-server)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg?style=flat-square)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/qscgy5713/Lazarus?style=flat-square)](https://goreportcard.com/report/github.com/qscgy5713/Lazarus)

證明你的資料庫備份**真的能還原**——不是「備份腳本有跑完」，是真的把 dump 灌進一個乾淨的資料庫、確認資料真的在裡面。

## 為什麼需要這個

大部分的備份監控在回答「備份有沒有產生」。但實際出事那天，真正讓人崩潰的是這幾種情況：

- 備份腳本三個月前就靜默失敗了，每天產出 0 bytes 的檔案
- 壓縮過程損毀，`.gz` 根本解不開
- 磁碟滿了，dump 被截斷在一半
- **最陰險的：`--schema-only` 參數不小心加上去了，備份還原起來完全成功，但是一張空表**

這些情境有一個共同點：**檔案都存在、大小看起來也「有東西」、備份任務的 exit code 都是 0**。只有真的跑一次還原才會發現。

Lazarus 就是定期幫你跑那一次還原。

```text
  ┌────────────────────────────────────────────────────────┐
  │                    Backup Sources                      │
  │        (AWS S3 / GCP GCS / SFTP / Local Mount)         │
  └───────────────────────────┬────────────────────────────┘
                              │ fetch_command / path
                              ▼
  ┌────────────────────────────────────────────────────────┐
  │                   Lazarus Runner                       │
  │        (Scheduled Cron / CI / GitHub Action)           │
  └───────┬───────────────────┬───────────────────┬────────┘
          │ 1. Spawns         │ 2. Restores       │ 3. Asserts
          ▼                   ▼                   ▼
 ┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
 │ Postgres / MySQL│ │  Redis Sandbox  │ │  SQLite Sandbox │
 │   (Disposable)  │ │   (RDB Verify)  │ │ (Ephemeral Copy)│
 └────────┬────────┘ └────────┬────────┘ └────────┬────────┘
          │                   │                   │
          └───────────────────┼───────────────────┘
                              │ 4. Verification Report
                              ▼
  ┌────────────────────────────────────────────────────────┐
  │             Lazarus Control Plane (Web UI)             │
  │  * Continuous Status Dashboard (Pass / Fail / Overdue) │
  │  * Dead Man's Snitch (Silent Failure Detection)        │
  │  * Compliance Audit Report Export (SOC 2 / ISO 27001)  │
  │  * Real-Time Incident Alerts (Slack / Discord)         │
  └────────────────────────────────────────────────────────┘
```

## 運作方式

對每個設定的目標：

1. （選用）跑 `fetch_command` 把備份從遠端抓到本機（見下方「遠端備份來源」）
2. 找到最新的備份檔（支援 glob，挑修改時間最新的——那才是你真的會拿來救命的那份）
3. 檢查新鮮度（太舊的備份就算能還原也是失敗的備份）
4. 對 PostgreSQL / MySQL / Redis：起一個**用完就丟**的 Docker 資料庫容器；對 SQLite：直接複製一份用完即丟的檔案（見下方「SQLite 不需要 Docker」）
5. 真的把備份還原進去（GPG 加密的備份會先自動解密，見下方「GPG 加密備份」）
6. 跑你定義的 SQL 斷言，確認資料真的在
7. 拆掉容器（SQLite 則是刪掉暫存複本）

全部通過 exit code 才是 0，方便直接塞進 cron 或 CI。多個目標預設會同時驗證（見下方「使用」一節的 `parallelism` 說明），彼此完全獨立、互不影響。

## 安裝

驗證 PostgreSQL / MySQL 備份需要本機可用的 Docker；SQLite 不需要。

從 [Releases](https://github.com/qscgy5713/Lazarus/releases) 下載對應平台的預編譯執行檔（Linux/macOS/Windows，amd64/arm64），解壓縮即可用：

```bash
tar xzf Lazarus_*_linux_amd64.tar.gz
./lazarus --version
```

或者從原始碼建置（需要 Go 1.24+）：

```bash
git clone https://github.com/qscgy5713/Lazarus.git
cd Lazarus
go build -o lazarus ./cmd/lazarus
```

## 5 秒快速體驗（零依賴）

想在不用 Docker、不用雲端金鑰的狀況下立刻親眼看看效果？直接跑專案內附的示範腳本：

```bash
./examples/quickstart.sh
```

腳本會自動建立一個暫時的 SQLite 資料庫、模擬真實備份檔並執行 SQL 斷言演練，輸出 `PASS quickstart-sqlite` 並在結束後自動清理暫存檔。

## 使用

```bash
cp lazarus.example.yml lazarus.yml   # 改成你的備份路徑
./lazarus --config lazarus.yml
```

輸出：

```
PASS  production-postgres (5.3s)
      backup: /backups/postgres/shop-2026-09-17.sql.gz (14.4 KB, 2h13m old)
      restore took: 1m42s
      check ok     users table is populated (= 1841)
      check ok     orders from the last week made it in (= 327)

FAIL  production-mysql [checks] check "customers table is populated" failed: got 0, want at least 1
      backup: /backups/mysql/app-latest.sql (2.1 KB, 1h02m old)
      restore took: 892ms
      check FAILED customers table is populated (got 0, want at least 1)

1 passed, 1 failed
```

指令選項：

| 參數 | 預設 | 說明 |
|---|---|---|
| `--config` | `lazarus.yml` | 設定檔路徑 |
| `--target` | (全部) | 只驗證指定的一個目標 |
| `--tag` | (全部) | 只驗證符合特定標籤（如 `prod`、`aws`、`staging`）的目標 |
| `--chaos` | `false` | 備份混沌工程演練模式：刻意損壞第一份備份，檢驗 Fallback 回退救援與告警自癒抗受性 |
| `--live` | `true` | 在互動式終端機即時呈現多目標並行進度列（非 TTY 或 `--json`/`--quiet` 時自動停用） |
| `--output-html` | (無) | 產出自包含且可離線/列印的正式 DR 災難復原審計 HTML 報告路徑 |
| `--daemon` | `false` | 以常駐守護進程模式持續定期輪詢演練（免手動設定 crontab） |
| `--interval` | (目標設定) | 守護進程演練間隔（如 `1h`、`30m`，覆蓋個別目標設定） |
| `--json` | `false` | 機器可讀的輸出，給 CI/腳本用 |
| `--quiet` | `false` | 只印出失敗的目標跟最後的統計，通過的目標完全不提（跟 `--json` 一起用時被忽略——JSON 本來就是給機器解析的完整資料） |
| `--keep-on-failure` | `false` | 目標失敗時保留 sandbox 容器（或 SQLite 暫存檔）不清掉，方便直接連進去查資料（見下方「保留失敗現場除錯」） |
| `--check-config` | `false` | 只檢查設定檔對不對就結束，不抓備份、不還原、完全不碰 Docker（見下方「設定檔檢查」） |
| `--version` | `false` | 印出版本後結束，不需要設定檔 |

環境變數：

| 變數 | 說明 |
|---|---|
| `LAZARUS_WEBHOOK_URL` | 通知用的 webhook URL，會覆寫設定檔裡的值（見下方「失敗通知」） |
| `LAZARUS_API_KEY` | Webhook / Control Plane 驗證金鑰 Bearer token，會覆寫設定檔裡的值 |
| `LAZARUS_GPG_PASSPHRASE` | 解密 GPG 加密備份用的密語（見下方「GPG 加密備份」）。沒有加密備份就不用設 |

Lazarus 會在 `state_file`（預設 `lazarus-state.json`）記錄每個目標上一次「完整通過驗證」的備份大小，用來支援下方的「備份大小驟變偵測」。這個檔案可以隨時刪除——下次執行就會重新從零開始建立基準值。

多個目標預設會同時驗證，最多 4 個一起跑（`parallelism`，可調整，設成 `1` 就變回一個一個跑）。每個目標本來就是完全獨立的（各自的 sandbox 容器，或各自的 SQLite 暫存複本），彼此不會互相干擾——只是縮短目標一多時整體要等的時間。輸出順序永遠跟設定檔裡的順序一致，跟實際完成的先後順序無關。

Exit code：`0` 全部通過、`1` 有驗證失敗、`2` 設定檔或參數有問題。

### 保留失敗現場除錯

check 失敗時，容器（或 SQLite 暫存複本）預設會被立刻拆掉——只留下錯誤訊息，想進一步查是哪裡少了資料，得自己手動重跑一次還原再摸索。加上 `--keep-on-failure`，任何在**還原成功之後**才發生的失敗（RTO 超時、check 失敗）都會保留現場，並在輸出裡印出可以直接貼上執行的指令：

```
FAIL  shop [checks] check "users populated" failed: got 0, want at least 1
      backup: /backups/shop.sql (997 B, 0s old)
      restore took: 133ms
      check FAILED users populated (got 0, want at least 1)
      kept for inspection: docker exec -it lazarus-verify-1789637480837239000-1 psql --username lazarus --dbname lazarus_verify
      (remember to clean it up yourself when done: docker rm -f, or delete the temp file)
```

幾個重點：

- 只有 sandbox 容器（或 SQLite 暫存複本）已經建立之後才發生的失敗會保留——`restore`、`rto`、`checks` 都算；`fetch`、`locate`、`age`、`size_drift` 這些階段連容器都還沒起，沒有現場可留。連 `restore` 本身失敗都會保留是刻意的：即使還原中途就因為語法錯誤而中斷，容器裡通常還是留著中斷前已經執行成功的部分，那往往才是你想知道「到底跑到哪裡開始壞掉」的線索
- 只有**失敗**的目標會保留，通過的目標一律照常清掉，不會留下一堆用不到的容器
- 保留下來的容器**不會自動清理**，用完要自己 `docker rm -f`（或刪掉印出的 SQLite 暫存檔路徑）——這是刻意的除錯用選項，不是預設行為
- sandbox 容器沒有對外開放連接埠，所以是透過 `docker exec` 連進容器內部查詢，不是從外部直接用 psql/mysql 連線

### 設定檔檢查

設定選項一多，打錯字、漏填、選項之間互相矛盾這些問題，現在得等真的跑一次（可能要先跑完 `fetch_command`、起完 Docker 容器）才會發現。`--check-config` 只做語法跟邏輯檢查就結束：

```bash
lazarus --config lazarus.yml --check-config
```

```
lazarus: config OK — 2 target(s), parallelism 4, state file "lazarus-state.json"
notify: format=slack when=on_failure webhook=not set

- production-postgres (postgres)
    path: /backups/shop-*.sql.gz
    image: postgres:16-alpine
    max_age: 26h0m0s
    max_restore_duration: 2h0m0s
    size_drift: max_decrease_pct=50%
    checks: 3

- local-sqlite (sqlite)
    path: /backups/app.db
    checks: 1
```

輸出的不只是「有沒有錯」，還包含實際生效的值——像是套用了哪個預設 image、`fetch_timeout` 沒設定時實際會是多少——這些原本要等真的執行過一次才看得到。

`--check-config` 完全不會執行 `fetch_command`、不會啟動任何 Docker 容器、也不會去看 `path` 指到的檔案存不存在——這些都要等真的執行才驗證得到，故意留給真正的驗證，適合放進 CI 快速檢查設定檔改動有沒有寫錯。Exit code 跟平常一樣：`0` 設定沒問題、`2` 設定有誤。可以搭配 `--target` 只檢查單一目標，也吃 `--json` 印出機器可讀的版本（給要用腳本解析結果的 CI 步驟用）。

## 設定

完整範例見 [`lazarus.example.yml`](lazarus.example.yml)。

```yaml
targets:
  - name: production-postgres
    engine: postgres                # postgres、mysql、sqlite、redis 或 mongodb
    # fetch_command: aws s3 cp ...   # 備份不在本機時才需要（見下方說明）
    path: /backups/shop-*.sql.gz    # 支援 glob，取最新的
    max_age: 26h                    # 超過這個年齡就算失敗
    max_restore_duration: 2h        # 還原本身超過這個時間就算失敗（見下方 RTO 說明）
    # memory_limit: 2g              # 選用：限制 sandbox 記憶體，防止大備份 OOM
    # cpus: "1.5"                   # 選用：限制 sandbox CPU 配額
    size_drift:
      max_decrease_pct: 50          # 比上次「完整通過」的備份小超過 50% 就算失敗（見下方說明）
    image: postgres:16-alpine       # sandbox 用的 image，要對應你的正式版本
    checks:
      - name: users table is populated
        sql: SELECT count(*) FROM users
        expect_min: 1
```

**checks 是這個工具的重點**。只檢查「還原成功」會漏掉最危險的情境（空殼備份），所以每個目標都該至少有一個「這張表要有資料」的斷言。

檢查的 SQL 要回傳**單一數字**，可以用三種期望值：

- `expect_min`：至少要有多少（最常用，`expect_min: 1` = 這張表不能是空的）
- `expect_max`：最多多少
- `expect_equal`：剛好等於多少
- `expect_pattern`：**正向正則表達式斷言**，文字結果必須匹配正則模式（例如版本格式驗證 `^v?\d+\.\d+\.\d+`）
- `expect_not_pattern`：**資料脫敏防洩漏斷言**，結果**絕對不可匹配**正則模式（例如檢查測試/匯出備份中的個資欄位是否確實已遮罩，若洩漏未脫敏 Email `[a-zA-Z0-9._%+-]+@gmail\.com` 則判定演練失敗）

```yaml
checks:
  # 正則匹配：驗證版本號或結構字串
  - name: migration version is semver
    sql: SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1
    expect_pattern: "^v?\\d+\\.\\d+\\.\\d+"

  # 資料脫敏校驗：防止敏感個資洩漏到 staging/備份中
  - name: user emails are properly anonymized
    sql: SELECT email FROM users WHERE email LIKE '%@gmail.com' LIMIT 1
    expect_not_pattern: "[a-zA-Z0-9._%+-]+@gmail\\.com"
```

### 還原時間上限（RTO）

`max_restore_duration` 只算「還原」那一步本身花的時間（不含起 sandbox、跑 checks）。這個數字平常沒有人知道——大部分團隊第一次量到真實的還原時間，是真的出事、正在等資料庫救回來的那一刻。

```yaml
max_restore_duration: 2h   # 服務最多只能忍受停機 2 小時，還原超過就算失敗
```

不設也沒關係，還原耗時每次都會顯示在輸出裡（`restore took: 1m42s`），純粹當參考資訊。

### 備份大小驟變偵測

checks 只能回答「有沒有資料」，答不了「資料量有沒有不對勁地變少」。如果上游的備份腳本被誰改壞了（例如不小心加了個過濾條件、只匯出了一部分資料表），還原出來的資料庫可能剛好還是能通過 `expect_min: 1` 這種寬鬆的斷言——表不是空的，只是資料少了 95%。

```yaml
size_drift:
  max_decrease_pct: 50   # 比上一次「完整通過驗證」的備份小超過 50% 就算失敗
```

比較基準是**上一次完整通過驗證**（還原成功、所有 checks 也過）的備份大小，記錄在 `state_file` 裡。這代表：

- 目標第一次執行時還沒有基準值，只會建立基準，不會失敗
- 只有失敗的那次不會更新基準值——一份真的壞掉的備份不會把及格線悄悄拉低，直到有人正視這個警告為止
- 只看「變小」，備份變大不會觸發（資料只會越長越多是正常的）

不設定的話（預設）完全不檢查，行為跟舊版一樣。

### 遠端備份來源

`path` 預設查找本機路徑——但大部分正式環境的備份根本不會放在跑 Lazarus 的這台機器上，通常是丟在 S3、專門的備份主機，或其他地方。Lazarus 提供兩種方式抓取遠端備份：

#### 方式一：原生 S3 / Cloudflare R2 / MinIO 物件儲存（免安裝 aws-cli）

Lazarus 內建純 Go AWS SigV4 認證客戶端，無須在主機或容器內預先安裝 `aws-cli` 或 Python 環境：

```yaml
targets:
  - name: production-postgres
    engine: postgres
    path: /backups/postgres/shop-latest.sql.gz
    s3:
      bucket: my-backups
      key: postgres/shop-latest.sql.gz
      region: us-east-1                  # 預設 us-east-1
      # endpoint: https://<account>.r2.cloudflarestorage.com  # Cloudflare R2 或 MinIO 請指定 endpoint
      # access_key_id: "..."             # 亦可透過環境變數 AWS_ACCESS_KEY_ID 注入
      # secret_access_key: "..."         # 亦可透過環境變數 AWS_SECRET_ACCESS_KEY 注入
```

#### 方式二：原生 Google Cloud Storage（GCS，免安裝 gcloud CLI）

Lazarus 內建純 Go JWT/OAuth2 服務帳戶認證客戶端，支援直接拉取 GCS Bucket 物件：

```yaml
targets:
  - name: gcp-postgres
    engine: postgres
    path: /backups/postgres/gcp-latest.sql.gz
    gcs:
      bucket: my-company-backups
      object: postgres/gcp-latest.sql.gz
      # credentials_json: '{"type":"service_account",...}' # 亦可透過 GOOGLE_APPLICATION_CREDENTIALS 注入
```

#### 方式三：原生 Azure Blob Storage（免安裝 az CLI）

Lazarus 內建純 Go Azure SharedKey HMAC-SHA256 認證客戶端，支援直接拉取 Azure 儲存體容器中的 Blob：

```yaml
targets:
  - name: azure-mysql
    engine: mysql
    path: /backups/mysql/azure-latest.sql.gz
    azure:
      account_name: mybackupstorage
      container: mysql-backups
      blob: azure-latest.sql.gz
      # account_key: "..."               # 亦可透過環境變數 AZURE_STORAGE_KEY 注入
```

#### 方式四：自訂 Shell 指令（`fetch_command`）

`fetch_command` 讓你在 `path` 被查找之前，先跑一段 shell 指令把備份抓到本機：

```yaml
fetch_command: aws s3 cp s3://my-backups/postgres/shop-latest.sql.gz /backups/postgres/shop-latest.sql.gz
fetch_timeout: 5m   # 預設 5 分鐘，避免抓取卡住讓 cron 無限等下去
```

`fetch_command` 完整透過 shell 執行（`sh -c`），管線、重導向、`&&` 串接指令都能用——你原本怎麼手動抓這份備份，這裡幾乎原封不動搬過來就能用（`aws s3 cp`、`scp`、`rclone`、`rsync` 都可以）。指令執行失敗（non-zero exit）會直接讓目標在 `fetch` 這個階段失敗，`path` 連查都不會查；指令自己印出的錯誤訊息（例如 `Access Denied`）會被原封不動保留在失敗訊息裡，方便直接定位問題。

抓取需要的認證（AWS 憑證、SSH key 等）要讓執行 Lazarus 的那個行程本身拿得到——注意 cron 通常不會載入你 shell 的環境變數，需要另外設定。

#### 自動清理暫存備份檔（`cleanup_backup`）

當透過遠端指令或 S3 拉取備份到本機時，驗證結束後若不希望備份檔佔用磁碟空間，可開啟 `cleanup_backup: true`，Lazarus 會在該目標演練完成後自動刪除本機備份檔：

```yaml
targets:
  - name: production-postgres
    engine: postgres
    path: /tmp/shop-latest.sql.gz
    s3:
      bucket: my-backups
      key: postgres/shop-latest.sql.gz
    cleanup_backup: true   # 演練完畢自動刪除本機 /tmp/shop-latest.sql.gz
```

> [!IMPORTANT]
> 為了防範誤刪本機主要的輪替備份檔案，`cleanup_backup: true` **嚴禁**與包含萬用字元（如 `*`、`?`、`[`）的 Glob 路徑混用；且必須搭配遠端拉取（`s3` 或 `fetch_command`），`path` 必須為具體確定的單一檔案路徑。

#### S3 下載即時進度與速率

當透過 `s3:` 拉取大體積備份時，終端機將即時顯示傳輸進度、百分比與下載速率（例如 `[s3] downloading... 45.2/120.0 MB (37%) at 15.4 MB/s`），避免長時間等待時產生無回應疑慮。

不設定 `fetch_command` 或 `s3` 的目標行為完全不變，`path` 直接當本機路徑查找。

## 支援的備份格式

| 格式 | 說明 |
|---|---|
| PostgreSQL 純 SQL | `pg_dump` 的預設輸出，用 `psql` 還原 |
| PostgreSQL 自訂格式 | `pg_dump -Fc`，自動偵測（`PGDMP` 魔術位元組）並改用 `pg_restore` |
| MySQL 純 SQL | `mysqldump` 的輸出 |
| SQLite | 資料庫檔案本身的完整複本（不是 `.dump` 出來的 SQL 文字），沒有伺服器可以匯入，本來就是一個獨立檔案 |
| Redis RDB | Redis 二進位快照檔（`.rdb`），自動偵測魔術位元組 `REDIS`，透過拋棄式容器加載並執行完整性校驗 |
| MongoDB Archive | MongoDB BSON 封裝檔（`.archive` 或 `.archive.gz`），透過拋棄式 `mongo:7.0` 容器以 `mongorestore` 還原並以 `mongosh` 執行斷言 |
| gzip 壓縮 | 以上任一種加上 `.gz`，串流解壓縮，不佔額外磁碟空間 |
| Zstandard (zstd) 壓縮 | 以上任一種加上 `.zst` 或 `.zstd`，超高速串流解壓縮，解壓速度比 gzip 快 3~5 倍 |
| GPG 加密 | 以上任一種（含已經 gzip / zstd 壓縮過的）加上 `.gpg`/`.pgp`/`.asc`，自動偵測並解密 |

### SQLite 不需要 Docker

PostgreSQL / MySQL 的備份是 SQL 文字，需要一個真的資料庫伺服器把它「還原」回去才能驗證。SQLite 的備份直接就是資料庫檔案本身——沒有匯入這一步。所以驗證 SQLite 目標時，Lazarus 完全不會碰 Docker：把備份複製（視需要先解壓縮）到一個用完即丟的暫存檔案，對這份複本跑 SQLite 自己的 `PRAGMA integrity_check`，再執行你設定的 checks。

```yaml
targets:
  - name: local-sqlite
    engine: sqlite
    path: /backups/sqlite/app-*.db
    checks:
      - name: users table is populated
        sql: SELECT count(*) FROM users
        expect_min: 1
```

`max_age`、`max_restore_duration`、`size_drift` 這些設定對 SQLite 目標一樣有效——只有 `image` 用不到（沒有容器）。

### Redis 備份驗證

Redis 備份是二進位的 RDB 檔案（如 `dump.rdb` 或 `dump.rdb.gz`）。Lazarus 會自動解壓並掛載至拋棄式 `redis:7-alpine` 沙盒容器，由 Redis 實例自動載入並執行 `redis-check-rdb` 完整性校驗。

斷言檢查支援任何回傳單一整數的 Redis 命令（可使用 `command` 或 `sql` 欄位，例如 `DBSIZE`、`HLEN`、`SCARD`、`ZCARD`、`LLEN`）：

```yaml
targets:
  - name: cache-redis
    engine: redis
    path: /backups/redis/dump-*.rdb
    checks:
      - name: keys exist in database
        command: DBSIZE
        expect_min: 1
      - name: active sessions count
        command: HLEN user_sessions
        expect_min: 10
```

### GPG 加密備份

很多正式環境的備份基於合規要求會用 GPG 加密存放。`path` 指到的檔案只要副檔名是 `.gpg`、`.pgp` 或 `.asc`（不分大小寫），Lazarus 會在還原前自動用 `gpg` 解密——不需要在設定檔裡多寫任何東西，純粹看檔名判斷。

```bash
LAZARUS_GPG_PASSPHRASE=your-passphrase lazarus --config lazarus.yml
```

密語只能透過 `LAZARUS_GPG_PASSPHRASE` 環境變數提供，設定檔裡沒有對應欄位——這樣密語就不可能不小心被 commit 進版本控制。如果備份是用已經匯入本機 GPG 金鑰圈的公鑰加密、而且私鑰沒有另外設密語保護，可以完全不設這個環境變數。

檔名判斷同時支援「先壓縮再加密」這種常見慣例（例如 `shop.sql.gz.gpg`）——`.gpg` 拿掉之後剩下的 `shop.sql.gz` 一樣會被正確辨識出需要解壓縮。解密永遠先於解壓縮執行。

沒設定 `LAZARUS_GPG_PASSPHRASE`、備份卻真的需要密語解密時，`gpg` 會在幾秒內乾脆地失敗（`[restore]` 階段），不會卡住等人互動輸入密碼。

解密後的明文只會短暫存在一個權限收緊為 `0600`（僅擁有者可讀）的暫存檔裡，還原流程結束就刪除——加密的重點就是保護資料，解密的中繼過程沒道理讓同一台機器上的其他使用者看得到。

### 現代輕量密碼學 AGE 加密備份支援 (`.age`)

除了傳統 GPG 之外，Lazarus 原生支援雲原生社群廣泛採用的現代簡約加密工具 [`age`](https://github.com/FiloSottile/age)。只要檔案副檔名為 `.age`（例如 `backup.sql.gz.age` 或 `dump.rdb.age`），Lazarus 自動調用 `age --decrypt` 進行解密。

私鑰身份憑證支援以下兩種方式提供（亦可在 target 內設定 `age_identity`）：

```bash
# 方式 A：直接透過環境變數注入私鑰內容
LAZARUS_AGE_IDENTITY="AGE-SECRET-KEY-1..." lazarus --config lazarus.yml

# 方式 B：指定儲存 age 私鑰的私密金鑰檔案路徑
LAZARUS_AGE_KEY_FILE="/etc/lazarus/keys/key.txt" lazarus --config lazarus.yml
```

解密過程採用臨時獨立沙盒並使用嚴格許可權 `0600`，執行完畢後立即強制抹除暫存金鑰與解密檔。

### 差異與增量備份鍊回放演練 (`incremental_patches`)

企業級資料庫常採用「全量 Base 備份 + 週期性差異/增量 Dump」的備份策略。如果增量鍊中有任何一節損壞或遺失，整個災難復原都會宣告破產。

Lazarus 支援在 Base 備份成功還原後，自動搜尋並依檔案修改時間戳升冪（由舊到新）嚴格套用增量 SQL/Dump 補丁鍊：

```yaml
targets:
  - name: production-postgres
    engine: postgres
    path: /backups/postgres/base-*.sql.gz
    # 支援 glob 比對多個差異/增量補丁檔，自動依時間順序依序回放
    incremental_patches:
      - /backups/postgres/inc-*.sql.gz
```

若任何一個增量補丁在回放過程中失敗，演練立即標記為 `[restore]` 階段中斷並發出警報，報告中完整記錄 `Incremental Patches Applied` 清單。

### 沙盒真實資源開銷分析 (RAM Peak & Disk Footprint)

資料庫備份還原是高壓的 I/O 與記憶體密集型任務。Lazarus 在沙盒還原演練期間透過 cgroup 及容器環境實時採樣：
- **RAM Peak**：精確量測資料庫還原過程中的記憶體最高峰值，防範生產環境在災難復原時發生意外的 OOM Kill。
- **Disk Footprint**：量測資料解壓並寫入資料庫後在磁碟上的真實檔案目錄佔用（非僅僅壓縮檔大小），計算出精確的資料膨脹倍率。

指標會自動收錄於 CLI 輸出、JSON 審計報告、GitHub Step Summary 以及 Web 控制台儀表板。

### 災難應變自動修復與處置劇本 (`remediation`)

當演練失敗、發現備份無法還原或資料異常萎縮時，除了發送通知警報，Lazarus 支援自動觸發預先定義的緊急處置劇本（Automated Remediation Playbook）：

```yaml
targets:
  - name: production-postgres
    engine: postgres
    path: /backups/postgres/shop-*.sql.gz
    remediation:
      # 當演練失敗時自動執行緊急處置腳本
      command: "kubectl create job --from=cronjob/db-backup emergency-backup-$(date +%s)"
      trigger_on: failure   # "failure" (預設) 或 "critical_drift" (資料結構重大漂移時)
      timeout: 5m
```

處置腳本執行時會自動注入演練環境變數（`$LAZARUS_TARGET`, `$LAZARUS_STAGE`, `$LAZARUS_ERROR`, `$LAZARUS_BACKUP_PATH` 等），並在稽核報告中記錄處置結果與耗時。

## 失敗通知

只靠 exit code 的話，凌晨四點跑的 cron 發現備份壞了也沒人知道。設定 webhook 就能把結果送到 Slack / Discord：

```yaml
notify:
  format: slack        # slack | discord | telegram | teams | generic | lazarus | pagerduty | email
  when: on_failure     # on_failure | always | never

  # 原生 Email (SMTP) HTML 彙整郵件通報（當 format: email 時啟用）
  # smtp:
  #   host: smtp.mailgun.org
  #   port: 587
  #   username: postmaster@mg.example.com
  #   password: secret_password
  #   from: lazarus@example.com
  #   to:
  #     - alerts@example.com
  #     - dba-oncall@example.com
```

Webhook URL 建議用環境變數給，不要寫進設定檔：

```bash
LAZARUS_WEBHOOK_URL=https://hooks.slack.com/services/xxx ./lazarus --config lazarus.yml
```

- **Slack (Block Kit)**：具備標題 Header、狀態區塊與 Markdown 錯誤碼塊；當失敗且容器有輸出時自動附帶末尾 Stderr 日誌。設定 `dashboard_url` 時會自動加入「🌐 View Dashboard」按鈕導向控制台。
- **Discord (Rich Embeds)**：通過時顯示綠色邊框 (`#2ecc71`)，失敗時顯示紅色邊框 (`#e74c3c`)，並逐條列出每個目標的還原耗時與錯誤階段。
- **Telegram Bot**：原生 HTML 格式卡片，具備專屬狀態 Header 與 `<pre>` 等寬代碼區塊。
- **Microsoft Teams**：MessageCard / Adaptive Card 格式，支援色彩飾條（綠/紅）與 Markdown 清單；設定 `dashboard_url` 時附帶「🌐 View Dashboard」操作連結。
- **PagerDuty (Events API v2)**：演練失敗時觸發 `critical` Incident，全部通過時可發送 `info` 狀態。
- **原生 Email (SMTP)**：發送響應式美觀 HTML 晨報/演練報告，自帶目標狀態卡片、標籤徽章、檢查項目統計與錯誤診斷 Hint。支援標準 SMTP 驗證與自訂寄件者/收件者清單。

失敗時的告警卡片格式長這樣：

```text
🚨 Lazarus: Database Restoration Verification Failed
Status Summary:
FAIL  empty-shell-backup [checks] check "users have rows" failed: got 0, want at least 1
      backup: /backups/empty-shell.sql (2.1 KB, 1h02m old)
      restore took: 892ms
```

**`when: always` 值得考慮**：如果 Lazarus 自己停止運作了（cron 壞掉、機器關機），「沒收到通知」看起來跟「備份都很健康」一模一樣。每次都發通知能把這種沉默變成訊號——這正是這個工具在別的地方幫你解決的問題，套在它自己身上。

通知送不出去不會改變驗證的結果（exit code 仍然反映備份本身的狀態），但會在 stderr 明確警告，不會被靜默吞掉。

## 目標標籤分組與多環境管理（Tags & Multi-Environment Grouping）

在跨多雲（AWS、GCP、地端機房）或多環境（Production、Staging、Analytics）架構中，可在各目標設定 `tags` 屬性：

```yaml
targets:
  - name: production-postgres
    tags: [prod, aws, postgres]
    # ...

  - name: staging-mysql
    tags: [staging, gcp, mysql]
    # ...
```

- **CLI 篩選執行**：使用 `--tag` 旗標僅執行特定標籤分組的目標（例如 `./lazarus --tag prod` 或 `./lazarus --tag staging`）。
- **Control Plane 多維度篩選**：Web 控制台頂部自動彙整標籤晶片按鈕列，點擊即可切換單一或全部環境檢視。
- **審計紀錄依標籤匯出**：API `GET /api/v1/export/csv?tag=prod` 支援依標籤篩選歷史匯出清單，且 CSV 中包含 `Tags` 欄位。

## 內建排程守護進程（Daemon Mode Runner）

除了外部 cron 或 systemd timer，Lazarus 支援以常駐背景守護進程模式運作：

```bash
# 以守護進程模式啟動，依目標間隔（或預設 1 小時）循環演練
./lazarus --config lazarus.yml --daemon

# 覆蓋預設間隔為每 30 分鐘自動執行一次
./lazarus --config lazarus.yml --daemon --interval 30m

# 僅針對特定環境標籤常駐輪詢
./lazarus --config lazarus.yml --daemon --tag prod --interval 2h
```

守護進程特性：
- **即時初次演練**：進程啟動時立即觸發首次完整演練，隨後依據定時器自動循環。
- **優雅平滑關閉**：捕捉 `SIGINT` (Ctrl+C) 或 `SIGTERM` 信號，等待當前正在進行的 sandbox 還原或 check 斷言安全清理後乾淨退出。
- **持續通報整合**：每次循環依據 `notify` 策略自動向 Slack、Discord、Email 或 Control Plane 心跳回報。

## 企業級災難復原與安全特性 (Enterprise DR & Security)

### 1. 備份回退救援演練與真實 RPO 分析 (Fallback Disaster Recovery & RPO)

當最新的一份備份損毀（檔案截斷、GPG 損壞或校驗失敗）時，一般備份工具只會回報失敗。Lazarus 支援自動往前回退歷史備份（最多嘗試 `max_fallback_depth` 份）：
- 若回退的歷史備份還原並檢驗成功，演練將標記為 `FALLBACK PASS`。
- 精確計算出**真實可復原時間點差距 (Actual RPO)**，告知團隊「若現在發生真實災難，最近可救回的資料停留在多久之前」。

```yaml
targets:
  - name: production-db
    engine: postgres
    path: /backups/postgres/*.sql.gz
    fallback_on_failure: true
    max_fallback_depth: 3
```

### 2. 全表自動掃描與結構漂移比對 (Automated Schema & Table Drift Inspection)

還原完成後，自動探測現存所有資料表與 Row 數概覽，並可比對預期的 Baseline 清單：
- 自動抓出消失的關鍵資料表（Missing Tables）、全空的資料表（Empty Tables）。
- 支援 PostgreSQL、MySQL、SQLite 與 MongoDB。

```yaml
targets:
  - name: production-db
    engine: postgres
    path: /backups/postgres/*.sql.gz
    auto_schema_check: true
    schema_baseline: [users, orders, audit_logs, payments]
```

### 3. 沙盒安全隔離加固 (Network Isolation & Sandbox Hardening)

執行未知的歷史備份或第三方匯入檔時，防範潛在的 SQL 注入反彈 Shell、惡意儲存過程 (Stored Procedure) 或 SSRF 外連：
- `network: none`：對 Docker 沙盒容器進行完全斷網隔離。
- `read_only_rootfs: true`：掛載容器根檔案系統為唯讀，搭配 tmpfs 暫存。

```yaml
targets:
  - name: untrusted-import-db
    engine: postgres
    path: /backups/untrusted/*.sql.gz
    network: none
    read_only_rootfs: true
```

### 4. 演練前後生命週期勾子 (Pre/Post Drill Lifecycle Hooks)

在目標演練前後執行自訂腳本，支援超時保護與完整環境變數注入：
- 注入變數：`$LAZARUS_TARGET`, `$LAZARUS_ENGINE`, `$LAZARUS_STATUS`, `$LAZARUS_STAGE`, `$LAZARUS_ERROR`, `$LAZARUS_DURATION_MS`, `$LAZARUS_RESTORE_DURATION_MS`, `$LAZARUS_BACKUP_PATH`, `$LAZARUS_FALLBACK_USED`, `$LAZARUS_FALLBACK_RPO_SECONDS`。

```yaml
targets:
  - name: production-db
    engine: postgres
    path: /backups/postgres/*.sql.gz
    pre_drill_command: "echo 'Starting drill for $LAZARUS_TARGET' > /var/log/drills.log"
    post_drill_command: "curl -X POST https://internal-api/audit -d 'target=$LAZARUS_TARGET&status=$LAZARUS_STATUS'"
    hooks_timeout: 30s
```

### 5. Control Plane SSE 即時串流 (Realtime Server-Sent Events)

Lazarus Control Plane 後端提供 `GET /api/v1/stream` 原生 Server-Sent Events 串流，前端 Web UI 具備即時連線指示燈與無刷新動態更新：
- 任何演練報告上傳或手動觸發時，所有連線中的瀏覽器即時同步刷新卡片與指標。
- 支援 `?api_key=` 權限鑑權與定期 Ping 心跳保持連線。

### 6. 孤兒沙盒自動資源回收器 (Orphan Container Reaper)

在主機非預期重開機或遭 `kill -9` 強制中斷時，Lazarus 容器標註有 `lazarus.sandbox=true` 與時間戳記：
- 每次演練開始前自動在背景收割超過 2 小時的懸空孤兒容器與 SQLite 暫存檔，徹底杜絕磁碟洩漏。

### 7. 離線備份診斷與快速探測 (`lazarus inspect`)

免啟動 Docker，快速探測備份檔之魔術位元組、真實格式、壓縮方式與 SHA-256：

```bash
./lazarus inspect /backups/postgres/shop-latest.sql.gz
```

輸出範例：
```text
File:        /backups/postgres/shop-latest.sql.gz
Size:        14.4 KB (14745 bytes)
Age:         2h13m (modified 2026-09-17T06:00:00Z)
Format:      postgres-custom
Compression: gzip (compressed=true)
Encrypted:   false
MagicBytes:  1f8b080000000000
SHA-256:     e4d909c290d0fb1ca068ffaddf22cbd0...
```

### 8. 關鍵資料表哈希與精確字串比對 (`expect_string`)

在 checks 中支援精確字串比對，適合針對關鍵配置表或權限表進行 MD5/SHA256 哈希防竄改與防靜默覆蓋比對：

```yaml
checks:
  - name: system configs checksum matches baseline
    sql: SELECT md5(string_agg(key || '=' || value, ',' ORDER BY key)) FROM system_configs
    expect_string: "e4d909c290d0fb1ca068ffaddf22cbd0"
```

### 9. 互動式快速配置生成精靈 (`lazarus init`)

一鍵生成標準且包含最佳實踐的設定檔範本：

```bash
./lazarus init lazarus.yml
```

### 10. 備份混沌工程演練模式 (Backup Chaos & Mutation Testing: `--chaos`)

真正的災難復原演練不僅要驗證「備份完好時能跑通」，更要驗證「當主要備份檔損壞、被勒索軟體局部加密或網路傳輸損毀時，系統的 Fallback 回退救援機制與告警自癒是否真的能成功自救」：
- 在演練沙盒還原前，自動在暫存副本上故意注入 Byte Corruption（翻轉字節），絕不更動原始備份檔案。
- 驗證還原失敗是否被準確攔截，並驗證 Fallback 是否自動成功切換至前一份歷史備份救回資料。
- 演練報告中會自動標記 `🧪 Chaos Resilience Verified: PASS (RPO: X hours)`。

```bash
# 命令列一鍵對所有啟用 fallback 的目標啟動混沌測試
./lazarus --config lazarus.yml --chaos
```

### 11. 獨立 DR 審計 HTML 報告產出 (`--output-html` / `lazarus export`)

面對 SOC 2、ISO 27001、HIPAA 或內部資訊安全稽核時，一鍵匯出排版現代、深色/淺色自適應、可離線開啟且支援 `@media print` 轉存 PDF 的正式 DR 審計報告：
- 涵蓋所有 Target 的備份指紋、還原耗時、各項 SQL 斷言、結構漂移比對與 RPO 分析。

```bash
# 執行演練時同步產生正式 HTML 審計報告
./lazarus --config lazarus.yml --output-html dr-report.html

# 或從現有狀態一鍵導出報告
./lazarus export --config lazarus.yml --output dr-report.html --format html
```

### 12. 多目標並行即時動態終端介面 (Live Terminal Multi-Target Progress: `--live`)

在平行驗證多個目標時（如 `--parallel 4`），終端機會以 ANSI 原生動態狀態列即時呈現各資料庫的當前階段：
`[fetch]` ➔ `[sandbox]` ➔ `[restore]` ➔ `[schema]` ➔ `[checks]` ➔ `[done]`：
- 清楚展示當前各目標耗時與進度百分比；非 TTY 或 `--json`/`--quiet` 時自動安全降級為一般日誌。

### 13. Control Plane 30 天歷史 SLA / MTTR 純 SVG 趨勢圖

Control Plane 儀表板內建零外部相依性之原生純 SVG 圖表與 Target 歷史彈窗：
- **30-Day Drill Activity**：每日成功（綠色）與失敗（紅色）演練量堆疊柱狀圖，滑鼠懸停顯示當日詳情。
- **MTTR Performance Trend**：過去 30 天平均還原時間（Mean Time to Restore）走勢折線與漸層面積圖。
- **Target History Modal**：點擊任何資料庫卡片的 `History` 按鈕，即可查看最近 20 次演練的詳細歷程與耗時。

## 排進 cron

```cron
# 每天早上 6 點驗證備份
0 6 * * * cd /opt/lazarus && LAZARUS_WEBHOOK_URL=https://hooks.slack.com/services/xxx ./lazarus --config lazarus.yml
```

因為 exit code 有分好，也可以接到現有的監控系統上（例如
[ChronosMonitor](https://github.com/qscgy5713/ChronosMonitor) 之類的任務監控工具）。

目標數量一多、平常又幾乎都會通過時，`--quiet` 能讓 cron 寄來的信只在真的有問題時才有內容：

```cron
0 6 * * * cd /opt/lazarus && ./lazarus --config lazarus.yml --quiet
```

在 CI 裡對設定檔改動跑一次快速檢查，不需要 Docker：

```yaml
# .github/workflows 之類的地方
- run: lazarus --config lazarus.yml --check-config
```

## 用 systemd timer 排程

比 cron 多一點好處：機器關機時錯過的執行會在下次開機補跑（`Persistent=true`），日誌直接進 `journalctl` 不用自己接，失敗了也看得到明確的服務狀態。範例檔案在 [`examples/systemd/`](examples/systemd/)：

```bash
sudo cp examples/systemd/lazarus.service examples/systemd/lazarus.timer /etc/systemd/system/
sudo systemctl enable --now lazarus.timer
```

`lazarus.service` 用 `EnvironmentFile=-/opt/lazarus/lazarus.env` 讀取 webhook URL、GPG 密語這類密語（`-` 前綴代表檔案不存在也不報錯）——不要把這些寫進 unit 檔案本身，`/etc/systemd/system/` 底下的檔案預設是全機器可讀的：

```bash
# /opt/lazarus/lazarus.env
LAZARUS_WEBHOOK_URL=https://hooks.slack.com/services/xxx
```

查看執行紀錄：`journalctl -u lazarus.service`。手動觸發一次：`sudo systemctl start lazarus.service`。

若需將 **Lazarus Control Plane (Web UI)** 作為常駐系統服務運行，可使用 [`examples/systemd/lazarus-server.service`](examples/systemd/lazarus-server.service)：

```bash
sudo cp examples/systemd/lazarus-server.service /etc/systemd/system/
sudo systemctl enable --now lazarus-server
```

## 用 Docker Compose 部署

repo 根目錄的 [`docker-compose.yml`](docker-compose.yml) 預先配置好了兩個服務：
1. **`server`**：Lazarus Control Plane 視覺化儀表板與合規狀態中心（常駐背景服務）
2. **`lazarus`**：Lazarus CLI 驗證 runner（執行一次性還原演練）

### 啟動 Control Plane 監控儀表板

```bash
docker compose up -d server
```

服務預設在 `http://localhost:8080` 啟動，瀏覽器直接打開即可看到視覺化儀表板。Compose 服務已內建 `/readyz` 健康檢查探針（`healthcheck`）並以非 root 使用者 `lazarus` (UID 10001) 運行，符合安全加固規範。亦可直接使用 GitHub Container Registry (GHCR) 預建映像檔：

```bash
docker run -d --name lazarus-server -p 8080:8080 -v lazarus-data:/data ghcr.io/qscgy5713/lazarus-server:latest
```

### 執行備份還原演練

Lazarus runner 容器需要跟主機的 Docker daemon 通訊以啟動拋棄式 sandbox 資料庫容器，因此掛載了 `/var/run/docker.sock`：

```bash
cp lazarus.example.yml lazarus.yml   # 改成你的備份路徑
docker compose run --rm lazarus
```

Compose 本身不會幫你排程，`docker compose run --rm lazarus` 是跑完即結束——真正的定期執行可以交給主機的 cron、systemd timer，或透過下方介紹的 GitHub Actions 執行。

`state_file` 建議指到掛載的 volume 路徑（例如 `/var/lib/lazarus/lazarus-state.json`），不然每次 `--rm` 都會把 size-drift 的基準值一起丟掉。

如果備份是用**非對稱金鑰**（公鑰）加密而不是用 `LAZARUS_GPG_PASSPHRASE` 那種密碼式加密，容器每次啟動都是全新的、沒有金鑰圈——直接把 host 的 `~/.gnupg` 掛進去**行不通**：裡面的 gpg-agent socket 檔案沒辦法在容器裡正常運作（`gpg-agent` 連線會直接失敗），實際測過會噴 `Read-only file system` 或 `can't connect to the gpg-agent` 這類錯誤。正確做法是先匯出金鑰、用一次性指令匯入到一個獨立的 named volume 裡，之後每次執行都重複使用這個 volume：

```bash
gpg --export-secret-keys your-key-id > lazarus-key.asc

# 一次性匯入，之後就不用再做
docker compose run --rm --entrypoint sh \
  -v "$(pwd)/lazarus-key.asc:/tmp/key.asc:ro" \
  lazarus -c "gpg --batch --import /tmp/key.asc"
```

然後把 `docker-compose.yml` 裡註解掉的 `lazarus-gnupg` volume 取消註解（設定跟掛載都要），之後 `docker compose run --rm lazarus` 就會用這個持久化的金鑰圈解密。

## 🎛️ Lazarus Control Plane (Web UI) 儀表板

當備份驗證演練分散在多台主機、Kubernetes 叢集或多條 CI/CD 流水線時，**Lazarus Control Plane (Web UI)** 提供集中視覺化監控與災難復原合規治理。

```text
┌───────────────────────────────────────────────────────────────────────────────┐
│  Lazarus Control Plane (Web UI)             [ Compliance Certificate ]        │
│  Continuous Disaster Recovery & Reliability Assurance                         │
├───────────────────────────────────────────────────────────────────────────────┤
│  Tracked: 4 Targets   │ Passing: 3    │ Failing: 0    │ Overdue: 1 (Alert)    │
├───────────────────────────────────────────────────────────────────────────────┤
│  TARGET               ENGINE     STATUS    SIZE      RESTORE TIME  LAST DRILL │
│  production-postgres  postgres   PASS      14.4 KB   1m 42s        10m ago    │
│  analytics-mysql      mysql      PASS      2.1 KB    892ms         1h ago     │
│  cache-redis          redis      PASS      5.8 MB    340ms         3h ago     │
│  auth-sqlite          sqlite     OVERDUE   --        --            2d ago (!) │
├───────────────────────────────────────────────────────────────────────────────┤
│  [ Export Compliance Audit Report (SOC 2 Type II / ISO 27001) ]               │
└───────────────────────────────────────────────────────────────────────────────┘
```

### 核心功能

- **全時態健康儀表板 (Health Overview)**：直觀掌握各資料庫目標最新狀態（`PASS` / `FAIL` / `OVERDUE` / `MUTED`）、備份大小變化趨勢與還原耗時。
- **Web UI 即時日誌抽屜與終端機視窗 (Live Terminal Logs Drawer)**：在各目標卡片點擊「📋 Logs」即從右側滑出專業黑色終端機抽屜，以 Monospace 與顏色高亮呈現 LOCATE、BACKUP、SANDBOX、RESTORE、CHECKS 與 TEARDOWN 完整演練日誌串流，並支援「一鍵複製」與 ESC 快捷鍵關閉。
- **目標標籤分組與即時篩選 (Tag Chips & Filtering)**：依據目標標籤（如 `#prod`、`#staging`、`#aws`）在頂部動態生成晶片篩選按鈕列，支援即時多環境切換；CSV 匯出功能自動同步當前選取之標籤。
- **手動即時觸發演練 (On-Demand Drill Trigger)**：在 Web UI 目標卡片上一鍵點擊「⚡ Run Drill」或透過 API (`POST /api/v1/targets/{name}/trigger`) 即時觸發演練標記，支援手動驗收與即時輪詢。
- **RTO 還原耗時歷史趨勢圖 (Historical RTO Sparkline)**：在各目標卡片內建純 SVG 輕量趨勢曲線圖，即時呈現過去數次演練之還原耗時波動與成功/失敗節點。
- **維護模式與警報靜音 (Mute Alerts)**：當資料庫進行排程升級或停機維護時，可於 Web UI 或透過 API (`POST /api/v1/targets/{name}/mute`) 將目標一鍵切換為維護靜音模式，避免觸發誤報，並即時於狀態卡片與 Prometheus 指標同步。
- **歷史演練審計清單一鍵匯出 CSV (`/api/v1/export/csv`)**：提供歷史還原演練紀錄的 CSV 格式一鍵下載，包含演練時間戳、標籤、資料庫名稱、還原耗時、各項 checks 驗證筆數與斷言結果，支援 `?tag=` 篩選匯出，便於合規存檔與稽核檢驗。
- **企業合規審計原生 PDF 證書匯出 (`/api/v1/export/certificate.pdf`)**：純 Go 原生產出符合 PDF 1.4 標準的正式災難復原演練證書，具備動態 SHA-256 防篡改數位簽章與完整演練軌跡。SLA 達標率嚴格檢驗目標是否健康且還原耗時在 `sla_rto` 承諾之內（維護中的 Muted 目標自動排除於分母），符合 SOC 2、ISO 27001 與 HIPAA 合規審計要求。
- **容器失敗日誌回溯 (Container Stderr Tail)**：演練失敗時自動提取容器末 50 行 Stderr 日誌，直接呈現於 Web UI 終端機抽屜與 Slack/Teams 告警訊息中，無需手動 SSH 進主機即可一眼掌握崩潰原因。
- **Dead Man's Snitch（逾期靜默失效偵測）**：傳統監控只在腳本報錯時發出警報，但如果 crontab 被誤刪、伺服器離線或備份腳本死當，監控系統根本收不到任何通知。Control Plane 在目標超過預期時間（預設 26 小時）未收到還原報告時，自動標記為 `OVERDUE` 並亮起警報（處於維護靜音中的目標除外）。
- **合規稽核證明一鍵產生 (Audit Proof)**：內建合規報告匯出功能，將歷史還原紀錄整合成具時間戳記與資料筆數校驗的災難復原演練報告，直接提供給 SOC 2 Type II、ISO 27001 或金融監管稽核人員。
- **Prometheus 指標暴露 (`/metrics`) 與開箱即用 Grafana 儀表板**：原生暴露標準 Prometheus Exporter 端點，包含各目標還原耗時 (`lazarus_target_restore_duration_seconds`)、健康狀態 (`lazarus_target_status`，含 muted=3) 與統計指標。專案於 [`examples/grafana/lazarus-dashboard.json`](examples/grafana/lazarus-dashboard.json) 提供預先配置好的 Grafana 視覺化儀表板，支援一鍵匯入。
- **Kubernetes 原生健康探針 (`/healthz` 與 `/readyz`)**：符合雲原生標準，提供存活探針（Liveness: `/healthz`，輸出運行時間與目標數）與就緒探針（Readiness: `/readyz`，檢驗狀態儲存可用性）。
- **純 Go 輕量單一執行檔**：無需額外架設 PostgreSQL/MySQL 或 Redis，自帶內嵌 Web 介面與持久化狀態，資源消耗低於 20MB RAM。

### 獨立執行檔啟動

除了 Docker Compose 外，也可以直接以執行檔啟動 Control Plane：

```bash
# 建置並啟動服務
go build -o lazarus-server ./cmd/server
./lazarus-server -addr :8080 -api-key "your-super-secret-key" -overdue 26h
```

參數說明：
- `-addr`: HTTP 監聽位址（預設 `:8080`，可透過環境變數 `PORT` 或 `ADDR` 設定）
- `-api-key`: Webhook 驗證金鑰（可透過環境變數 `SERVER_API_KEY` 設定；支援 `X-API-Key`、`X-Lazarus-Key` 或 `Authorization: Bearer <token>`）
- `-state`: 狀態持久化 JSON 檔案路徑（預設 `lazarus-server.json`）
- `-overdue`: 逾期標記閥值時間（預設 `26h`）
- `-demo`: 啟用示範模式（預載代表性演練資料，亦可透過環境變數 `DEMO_MODE=true` 設定）
- `-alert-webhook`: Dead Man's Snitch 主動推播 Webhook URL（亦可透過環境變數 `ALERT_WEBHOOK_URL` 注入；當目標逾期且未靜音時主動發送 Slack / Discord / Teams 通知）
- `-alert-format`: 逾期告警訊息格式（預設 `slack`，支援 `slack`、`discord`、`teams`、`generic`，亦可透過環境變數 `ALERT_FORMAT` 設定）
- `-alert-interval`: 逾期檢查間隔（預設 `10m`，亦可透過環境變數 `ALERT_INTERVAL` 設定）

### 將 Lazarus 演練回報至 Control Plane

在 `lazarus.yml` 中設定 `notify`：

```yaml
notify:
  webhook_url: "http://control-plane.internal:8080/api/v1/reports"
  api_key: "your-super-secret-key"
  format: lazarus
  when: always   # 建議設為 always，確保無論成功或失敗均向 Control Plane 回報心跳狀態
```

或者直接透過環境變數注入（推薦在 CI / 排程環境中使用）：

```bash
export LAZARUS_WEBHOOK_URL="http://control-plane.internal:8080/api/v1/reports"
export LAZARUS_API_KEY="your-super-secret-key"
./lazarus --config lazarus.yml
```

---

## 🤖 官方 GitHub Action

若你的備份儲存在 AWS S3、Google Cloud Storage、Azure Blob，或是在 GitHub Actions 中排程備份，你可以直接使用官方 GitHub Action 在 GitHub-hosted runner 上自動啟動拋棄式容器進行演練，**零維護成本、無需專屬主機**。

### 使用範例（定期排程演練）

建立 `.github/workflows/verify-backup.yml`：

```yaml
name: Weekly Database Backup Restoration Drill

on:
  schedule:
    # 每週日清晨 04:00 UTC 定期驗證
    - cron: '0 4 * * 0'
  workflow_dispatch: # 支援在 GitHub 介面手動按鈕觸發

jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout Repository
        uses: actions/checkout@v4

      # 設定雲端存取憑證以供 fetch_command 下載備份
      - name: Configure AWS Credentials
        uses: aws-actions/configure-aws-credentials@v4
        with:
          aws-access-key-id: ${{ secrets.AWS_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_SECRET_ACCESS_KEY }}
          aws-region: us-east-1

      # 執行 Lazarus 備份還原演練
      - name: Run Lazarus Restore Drill
        uses: qscgy5713/Lazarus@main
        with:
          config: 'lazarus.yml'
          webhook-url: ${{ secrets.LAZARUS_WEBHOOK_URL }}
          api-key: ${{ secrets.LAZARUS_API_KEY }}
          gpg-passphrase: ${{ secrets.LAZARUS_GPG_PASSPHRASE }}
```

### Action 參數一覽

| 參數 | 預設值 | 說明 |
|---|---|---|
| `config` | `lazarus.yml` | 設定檔路徑 |
| `target` | (全部) | 若只需演練特定單一資料庫目標，填入目標名稱 |
| `version` | `latest` | 指定下載之 Lazarus 版本標籤（例如 `v1.0.0`） |
| `webhook-url` | (無) | 回報結果至 Control Plane 或 Webhook 的 URL |
| `api-key` | (無) | Control Plane 的驗證金鑰 |
| `gpg-passphrase` | (無) | 解密 GPG 加密備份用的密語 |

### Action 輸出

- `passed`: `true` 或 `false`。可用於後續工作步驟（例如演練失敗時觸發 PagerDuty 或建立 GitHub Issue）。

### GitHub Actions Step Summary 原生儀表板

Lazarus 原生支援 GitHub Actions 的 Step Summary。只要在 Actions 環境中執行，Lazarus 會自動偵測 `$GITHUB_STEP_SUMMARY` 並產生完整的 Markdown 演練審計表格，在 GitHub 工作階段摘要中直觀展示：
- 總體驗練合格狀態（`:white_check_mark: ALL DRILLS PASSED` 或 `:x: DRILLS FAILED`）
- 目標詳細表格（目標名稱、通過狀態、還原時間、備份體積、斷言通過數）
- 失敗目標的詳細錯誤訊息、斷言失敗原因以及容器 Stderr 日誌末尾回溯折疊區塊（`<details>`）

---

## 設計上的取捨

**為什麼用 Docker 容器而不是連到現有的測試資料庫？** 因為「乾淨」是驗證的前提。如果還原到一個已經有資料的資料庫，`SELECT count(*) FROM users` 回傳 100 根本無法判斷那是備份帶來的還是本來就在的。用完即丟的容器保證每次都從零開始。

**為什麼不直接用 `pg_restore --list` 看看檔案有沒有壞？** 那只驗證了檔案結構完整，不驗證資料。schema-only 的備份可以完美通過任何結構檢查。

**為什麼 checks 只支援單一數字？** 「有幾筆」幾乎能回答所有關於還原結果的問題，而且失敗訊息不會模稜兩可（`got 0, want at least 1` 比對比兩坨結果集清楚得多）。

**為什麼備份大小驟變偵測要另外存一個 state 檔，而不是塞進 checks？** checks 斷言的是「這次還原出來的資料庫」，天生沒有「跟上一次比較」的概念——它甚至不知道有沒有上一次。大小驟變偵測本質上是跨執行週期的比較，需要一個地方記住歷史，所以獨立成一個輕量的 JSON 檔，壞掉或刪除都不影響核心的還原驗證，只是重新歸零基準值而已。

**SQLite 為什麼不用 Docker 容器？** 因為 Docker 容器解決的問題（一個乾淨的伺服器可以把 SQL 匯入進去）對 SQLite 根本不存在——SQLite 的備份本身就是一份完整、獨立的資料庫檔案，沒有「匯入」這一步，複製到別處就已經是一份乾淨的副本了。硬是包一層容器只會多一份不必要的複雜度，不會多驗證到任何東西。

**為什麼 `fetch_command` 是一段 shell 指令，不是內建 S3/SFTP 客戶端？** 遠端儲存的種類沒有上限——S3、GCS、Azure Blob、公司內部的備份 API、單純一台用 scp 就能連的主機——每加一種都要新增一套設定選項跟一份程式碼，而且永遠追不完。讓使用者直接貼上自己已經在用的那行指令，Lazarus 不用認得任何一種儲存後端，也永遠不會漏掉某個團隊在用的冷門方案。

**為什麼 `--keep-on-failure` 保留下來的容器不會自動清掉？** 因為這個選項存在的唯一理由就是讓人有機會進去看——如果保留幾秒鐘後又自動拆掉，等於沒留。清理的責任交給使用者是刻意的：這是一個明確選用的除錯工具，不是預設行為，用的人本來就該預期要自己收尾。

**`--check-config` 為什麼不順便檢查 `path` 指到的檔案存不存在？** 因為這條界線畫在哪裡很清楚：`config.Load` 能做的是純粹看設定檔本身寫得對不對，跟環境完全無關；而檔案存不存在、`fetch_command` 抓不抓得到、Docker 起不起得來，答案都取決於「在哪台機器上跑」。在筆電上檢查一份要放到正式環境跑的設定檔時，備份檔案本來就不會在筆電上——這不代表設定檔寫錯了，混進來檢查反而會製造一堆假警報。

**GPG 密語為什麼只能透過環境變數給，設定檔裡完全沒有對應欄位？** 跟 webhook URL 是同一個道理，只是風險更高——備份的解密密語外流,等於備份裡的所有資料都直接曝光。設定檔常常會被 commit 進版本控制、複製到好幾台機器、貼到 issue 裡求助，任何一個欄位只要存在,就有被不小心留下來的風險。乾脆不給欄位,這個風險就從設計上排除掉,不用靠使用者自律。

## 開發

專案提供標準化 [`Makefile`](Makefile) 收錄常用開發工作流：

```bash
make build       # 編譯 lazarus CLI 與 lazarus-server 雙執行檔
make test        # 執行全庫單元測試
make test-race   # 執行包含並行競爭檢測 (-race) 之全庫測試
make lint        # 執行靜態程式碼分析 (go vet)
make fmt         # 自動排版 Go 程式碼 (gofmt)
make docker      # 建置 lazarus 與 lazarus-server 之 Docker 映像檔
make clean       # 清理本機編譯產物
```

PostgreSQL / MySQL / Redis 的端對端測試需要 Docker，會實際起容器、產生真實的 dump 再還原——這個工具的核心價值就是「真的跑一次」，所以驗證方式也一樣。SQLite 不需要 Docker，`go test` 裡就有跑真正的 `sqlite3` CLI、真的資料庫檔案的端對端測試（本機沒裝 `sqlite3` 會自動跳過）。

### 發布

推一個 `v*` 開頭的 tag（例如 `v1.0.0`）會觸發 GitHub Actions 用 [goreleaser](https://goreleaser.com/) 自動建置六種平台/架構組合（Linux/macOS/Windows × amd64/arm64）並發布到 GitHub Releases。本機可以先用以下指令跑一次乾跑，不需要 tag、不會真的發布：

```bash
goreleaser release --snapshot --clean --skip=publish
```

`.goreleaser.yaml` 的設定也會在每次 CI 跑的時候用 `goreleaser check` 跟一次單一平台的快照建置驗證過，設定檔壞掉不用等到真的推 tag 才發現。

## 授權

[MIT](LICENSE)
