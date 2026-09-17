# Lazarus

證明你的資料庫備份**真的能還原**——不是「備份腳本有跑完」，是真的把 dump 灌進一個乾淨的資料庫、確認資料真的在裡面。

## 為什麼需要這個

大部分的備份監控在回答「備份有沒有產生」。但實際出事那天，真正讓人崩潰的是這幾種情況：

- 備份腳本三個月前就靜默失敗了，每天產出 0 bytes 的檔案
- 壓縮過程損毀，`.gz` 根本解不開
- 磁碟滿了，dump 被截斷在一半
- **最陰險的：`--schema-only` 參數不小心加上去了，備份還原起來完全成功，但是一張空表**

這些情境有一個共同點：**檔案都存在、大小看起來也「有東西」、備份任務的 exit code 都是 0**。只有真的跑一次還原才會發現。

Lazarus 就是定期幫你跑那一次還原。

## 運作方式

對每個設定的目標：

1. （選用）跑 `fetch_command` 把備份從遠端抓到本機（見下方「遠端備份來源」）
2. 找到最新的備份檔（支援 glob，挑修改時間最新的——那才是你真的會拿來救命的那份）
3. 檢查新鮮度（太舊的備份就算能還原也是失敗的備份）
4. 對 PostgreSQL / MySQL：起一個**用完就丟**的 Docker 資料庫容器；對 SQLite：直接複製一份用完即丟的檔案（見下方「SQLite 不需要 Docker」）
5. 真的把備份還原進去
6. 跑你定義的 SQL 斷言，確認資料真的在
7. 拆掉容器（SQLite 則是刪掉暫存複本）

全部通過 exit code 才是 0，方便直接塞進 cron 或 CI。多個目標預設會同時驗證（見下方「使用」一節的 `parallelism` 說明），彼此完全獨立、互不影響。

## 安裝

需要 Go 1.27+。驗證 PostgreSQL / MySQL 備份需要本機可用的 Docker；SQLite 不需要。

```bash
git clone https://github.com/qscgy5713/Lazarus.git
cd Lazarus
go build -o lazarus ./cmd/lazarus
```

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
| `--json` | `false` | 機器可讀的輸出，給 CI/腳本用 |
| `--keep-on-failure` | `false` | 目標失敗時保留 sandbox 容器（或 SQLite 暫存檔）不清掉，方便直接連進去查資料（見下方「保留失敗現場除錯」） |

環境變數：

| 變數 | 說明 |
|---|---|
| `LAZARUS_WEBHOOK_URL` | 通知用的 webhook URL，會覆寫設定檔裡的值（見下方「失敗通知」） |

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

## 設定

完整範例見 [`lazarus.example.yml`](lazarus.example.yml)。

```yaml
targets:
  - name: production-postgres
    engine: postgres                # postgres、mysql 或 sqlite
    # fetch_command: aws s3 cp ...   # 備份不在本機時才需要（見下方說明）
    path: /backups/shop-*.sql.gz    # 支援 glob，取最新的
    max_age: 26h                    # 超過這個年齡就算失敗
    max_restore_duration: 2h        # 還原本身超過這個時間就算失敗（見下方 RTO 說明）
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

`path` 只認本機路徑——但大部分正式環境的備份根本不會放在跑 Lazarus 的這台機器上，通常是丟在 S3、專門的備份主機，或其他地方。`fetch_command` 讓你在 `path` 被查找之前，先跑一段 shell 指令把備份抓到本機：

```yaml
fetch_command: aws s3 cp s3://my-backups/postgres/shop-latest.sql.gz /backups/postgres/shop-latest.sql.gz
fetch_timeout: 5m   # 預設 5 分鐘，避免抓取卡住讓 cron 無限等下去
```

`fetch_command` 完整透過 shell 執行（`sh -c`），管線、重導向、`&&` 串接指令都能用——你原本怎麼手動抓這份備份，這裡幾乎原封不動搬過來就能用（`aws s3 cp`、`scp`、`rclone`、`rsync` 都可以）。指令執行失敗（non-zero exit）會直接讓目標在 `fetch` 這個階段失敗，`path` 連查都不會查；指令自己印出的錯誤訊息（例如 `Access Denied`）會被原封不動保留在失敗訊息裡，方便直接定位問題。

抓取需要的認證（AWS 憑證、SSH key 等）要讓執行 Lazarus 的那個行程本身拿得到——注意 cron 通常不會載入你 shell 的環境變數，需要另外設定。

不設定 `fetch_command` 的目標行為完全不變，`path` 直接當本機路徑查找。

## 支援的備份格式

| 格式 | 說明 |
|---|---|
| PostgreSQL 純 SQL | `pg_dump` 的預設輸出，用 `psql` 還原 |
| PostgreSQL 自訂格式 | `pg_dump -Fc`，自動偵測（`PGDMP` 魔術位元組）並改用 `pg_restore` |
| MySQL 純 SQL | `mysqldump` 的輸出 |
| SQLite | 資料庫檔案本身的完整複本（不是 `.dump` 出來的 SQL 文字），沒有伺服器可以匯入，本來就是一個獨立檔案 |
| gzip 壓縮 | 以上任一種加上 `.gz`，串流解壓縮，不佔額外磁碟空間 |

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

## 失敗通知

只靠 exit code 的話，凌晨四點跑的 cron 發現備份壞了也沒人知道。設定 webhook 就能把結果送到 Slack / Discord：

```yaml
notify:
  format: slack        # slack | discord | generic
  when: on_failure     # on_failure | always | never
```

Webhook URL 建議用環境變數給，不要寫進設定檔：

```bash
LAZARUS_WEBHOOK_URL=https://hooks.slack.com/services/xxx ./lazarus --config lazarus.yml
```

失敗時的訊息長這樣：

```
🔴 Lazarus: 1 of 2 backup(s) failed verification
• empty-shell-backup failed at `checks`
  check "users have rows" failed: got 0, want at least 1
  backup: /backups/empty-shell.sql
```

**`when: always` 值得考慮**：如果 Lazarus 自己停止運作了（cron 壞掉、機器關機），「沒收到通知」看起來跟「備份都很健康」一模一樣。每次都發通知能把這種沉默變成訊號——這正是這個工具在別的地方幫你解決的問題，套在它自己身上。

通知送不出去不會改變驗證的結果（exit code 仍然反映備份本身的狀態），但會在 stderr 明確警告，不會被靜默吞掉。

## 排進 cron

```cron
# 每天早上 6 點驗證備份
0 6 * * * cd /opt/lazarus && LAZARUS_WEBHOOK_URL=https://hooks.slack.com/services/xxx ./lazarus --config lazarus.yml
```

因為 exit code 有分好，也可以接到現有的監控系統上（例如
[ChronosMonitor](https://github.com/qscgy5713/ChronosMonitor) 之類的任務監控工具）。

## 設計上的取捨

**為什麼用 Docker 容器而不是連到現有的測試資料庫？** 因為「乾淨」是驗證的前提。如果還原到一個已經有資料的資料庫，`SELECT count(*) FROM users` 回傳 100 根本無法判斷那是備份帶來的還是本來就在的。用完即丟的容器保證每次都從零開始。

**為什麼不直接用 `pg_restore --list` 看看檔案有沒有壞？** 那只驗證了檔案結構完整，不驗證資料。schema-only 的備份可以完美通過任何結構檢查。

**為什麼 checks 只支援單一數字？** 「有幾筆」幾乎能回答所有關於還原結果的問題，而且失敗訊息不會模稜兩可（`got 0, want at least 1` 比對比兩坨結果集清楚得多）。

**為什麼備份大小驟變偵測要另外存一個 state 檔，而不是塞進 checks？** checks 斷言的是「這次還原出來的資料庫」，天生沒有「跟上一次比較」的概念——它甚至不知道有沒有上一次。大小驟變偵測本質上是跨執行週期的比較，需要一個地方記住歷史，所以獨立成一個輕量的 JSON 檔，壞掉或刪除都不影響核心的還原驗證，只是重新歸零基準值而已。

**SQLite 為什麼不用 Docker 容器？** 因為 Docker 容器解決的問題（一個乾淨的伺服器可以把 SQL 匯入進去）對 SQLite 根本不存在——SQLite 的備份本身就是一份完整、獨立的資料庫檔案，沒有「匯入」這一步，複製到別處就已經是一份乾淨的副本了。硬是包一層容器只會多一份不必要的複雜度，不會多驗證到任何東西。

**為什麼 `fetch_command` 是一段 shell 指令，不是內建 S3/SFTP 客戶端？** 遠端儲存的種類沒有上限——S3、GCS、Azure Blob、公司內部的備份 API、單純一台用 scp 就能連的主機——每加一種都要新增一套設定選項跟一份程式碼，而且永遠追不完。讓使用者直接貼上自己已經在用的那行指令，Lazarus 不用認得任何一種儲存後端，也永遠不會漏掉某個團隊在用的冷門方案。

**為什麼 `--keep-on-failure` 保留下來的容器不會自動清掉？** 因為這個選項存在的唯一理由就是讓人有機會進去看——如果保留幾秒鐘後又自動拆掉，等於沒留。清理的責任交給使用者是刻意的：這是一個明確選用的除錯工具，不是預設行為，用的人本來就該預期要自己收尾。

## 開發

```bash
go test ./... -race    # 單元測試，不需要 Docker
go build ./...
```

PostgreSQL / MySQL 的端對端測試需要 Docker，會實際起容器、產生真實的 dump 再還原——這個工具的核心價值就是「真的跑一次」，所以驗證方式也一樣。SQLite 不需要 Docker，`go test` 裡就有跑真正的 `sqlite3` CLI、真的資料庫檔案的端對端測試（本機沒裝 `sqlite3` 會自動跳過）。
