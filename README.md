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

1. 找到最新的備份檔（支援 glob，挑修改時間最新的——那才是你真的會拿來救命的那份）
2. 檢查新鮮度（太舊的備份就算能還原也是失敗的備份）
3. 起一個**用完就丟**的 Docker 資料庫容器
4. 真的把 dump 還原進去
5. 跑你定義的 SQL 斷言，確認資料真的在
6. 拆掉容器

全部通過 exit code 才是 0，方便直接塞進 cron 或 CI。

## 安裝

需要 Go 1.27+ 跟本機可用的 Docker。

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
      check ok     users table is populated (= 1841)
      check ok     orders from the last week made it in (= 327)

FAIL  production-mysql [checks] check "customers table is populated" failed: got 0, want at least 1
      backup: /backups/mysql/app-latest.sql (2.1 KB, 1h02m old)
      check FAILED customers table is populated (got 0, want at least 1)

1 passed, 1 failed
```

指令選項：

| 參數 | 預設 | 說明 |
|---|---|---|
| `--config` | `lazarus.yml` | 設定檔路徑 |
| `--target` | (全部) | 只驗證指定的一個目標 |
| `--json` | `false` | 機器可讀的輸出，給 CI/腳本用 |

Exit code：`0` 全部通過、`1` 有驗證失敗、`2` 設定檔或參數有問題。

## 設定

完整範例見 [`lazarus.example.yml`](lazarus.example.yml)。

```yaml
targets:
  - name: production-postgres
    engine: postgres              # postgres 或 mysql
    path: /backups/shop-*.sql.gz  # 支援 glob，取最新的
    max_age: 26h                  # 超過這個年齡就算失敗
    image: postgres:16-alpine     # sandbox 用的 image，要對應你的正式版本
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

## 支援的備份格式

| 格式 | 說明 |
|---|---|
| PostgreSQL 純 SQL | `pg_dump` 的預設輸出，用 `psql` 還原 |
| PostgreSQL 自訂格式 | `pg_dump -Fc`，自動偵測（`PGDMP` 魔術位元組）並改用 `pg_restore` |
| MySQL 純 SQL | `mysqldump` 的輸出 |
| gzip 壓縮 | 以上任一種加上 `.gz`，串流解壓縮，不佔額外磁碟空間 |

## 排進 cron

```cron
# 每天早上 6 點驗證備份，失敗時 cron 會把輸出寄給你
0 6 * * * cd /opt/lazarus && ./lazarus --config lazarus.yml
```

因為 exit code 有分好，也可以接到現有的監控系統上（例如
[ChronosMonitor](https://github.com/qscgy5713/ChronosMonitor) 之類的任務監控工具）。

## 設計上的取捨

**為什麼用 Docker 容器而不是連到現有的測試資料庫？** 因為「乾淨」是驗證的前提。如果還原到一個已經有資料的資料庫，`SELECT count(*) FROM users` 回傳 100 根本無法判斷那是備份帶來的還是本來就在的。用完即丟的容器保證每次都從零開始。

**為什麼不直接用 `pg_restore --list` 看看檔案有沒有壞？** 那只驗證了檔案結構完整，不驗證資料。schema-only 的備份可以完美通過任何結構檢查。

**為什麼 checks 只支援單一數字？** 「有幾筆」幾乎能回答所有關於還原結果的問題，而且失敗訊息不會模稜兩可（`got 0, want at least 1` 比對比兩坨結果集清楚得多）。

## 開發

```bash
go test ./... -race    # 單元測試，不需要 Docker
go build ./...
```

端對端測試需要 Docker，會實際起容器、產生真實的 dump 再還原——這個工具的核心價值就是「真的跑一次」，所以驗證方式也一樣。
