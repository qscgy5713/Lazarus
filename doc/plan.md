# 災難復原演練平台強化計畫 (Enterprise Plan)

## 目標 (Goal)
將 Lazarus 從單機/CI 執行的災難復原驗證工具昇華為「商用級分散式企業災難復原調度與管控平台 (Enterprise Distributed DR Orchestration Platform)」，消除商業落地的最後 8% 差距。

## 核心範疇 (Scope)

### 1. 企業級 RBAC 角色與 Token 鑑權 (Role-Based Access Control)
- **角色劃分**：
  - `RoleAdmin`：具備手動觸發演練 (`trigger`)、維護靜音 (`mute`)、註冊邊緣節點 (`register worker`) 與全功能操作權限。
  - `RoleViewer`：唯讀審計角色，僅允許檢視儀表板、即時日誌流、下載 CSV 報告與 PDF 合規證書。
- **向下相容**：Control Plane 若未配置 `AdminKey` 與 `ViewerKey`，預設開放給予管理權限，確保既有 CI/CD 與本機開發無縫升級。
- **Web 控制台整合**：頂部導覽列提供「🔑 Admin/Viewer」狀態與 Token 輸入對話框，儲存於瀏覽器 localStorage，未解鎖時阻止非 Admin 行為。

### 2. 分散式邊緣 Worker 註冊與任務調度 (Distributed Worker Registry & Dispatch)
- **邊緣 Runner 常駐模式**：支援 `lazarus --worker --control-plane=http://...`，自動向 Control Plane 註冊主機名稱、版本與支援之目標標籤 (Tags)。
- **心跳監控與過期踢除**：Worker 每 10 秒發送心跳維護在線狀態；Control Plane 超過 60 秒未收到心跳自動轉為 `offline`。
- **動態任務認領**：Web 控制台觸發「⚡ Run Drill」時標記 `TriggerPending`，Worker 輪詢 `/api/v1/workers/poll` 認領符合標籤的任務並在沙盒中執行。
- **Web 視覺化拓撲**：Web 控制台提供「👷 Workers」面板，即時呈現各邊緣節點的在線狀態、版本與當前執行任務。
- **Prometheus SRE 監控**：暴露 `lazarus_workers_online` 與 `lazarus_workers_total` 企業級指標。

## 設計步驟 (Phases)
1. 模型定義：定義 `UserRole`、`WorkerStatus`、`WorkerRecord`、`WorkerPollResponse`。
2. 存儲層擴充：在 `Store` 支援 Worker 登錄、心跳、清單查詢與標籤匹配認領。
3. 控制面 API 與中介層：新增 `/api/v1/auth/*` 與 `/api/v1/workers/*`，在敏感動作強制要求 `RoleAdmin`。
4. CLI 分散式 Worker 實作：新增 `--worker`、`--control-plane`、`--worker-id`、`--worker-token` 旗標與輪詢 goroutine。
5. Web 前端 UI：新增 Workers Modal、Auth Modal、Token 解鎖與權限提示。
6. 全面測試與驗證：RBAC 權限測試、Worker 註冊與輪詢認領測試、Prometheus 指標校驗。

## 驗收清單 (Acceptance Criteria)

以具體、可驗證的條件判斷完成度，取代主觀百分比。✅ 表示已有自動化測試涵蓋（單元測試或 `make e2e`）。

| # | 條件 | 狀態 | 驗證方式 |
|---|------|------|----------|
| 1 | 6 種資料庫（PostgreSQL plain/custom、MySQL、Redis、MongoDB、SQLite）的真實備份能還原並通過檢查 | ✅ | `make e2e` |
| 2 | schema-only 等「還原成功但沒資料」的備份會被判定失敗 | ✅ | `make e2e` 陷阱 target |
| 3 | GPG、age 加密備份可解密驗證，金鑰錯誤時明確失敗 | ✅ | `make e2e`、decrypt 單元測試 |
| 4 | S3（驗證 SigV4）、GCS、Azure 來源可下載驗證，憑證錯誤時明確失敗 | ✅ | `make e2e`（versitygw / fake-gcs / Azurite） |
| 5 | 最新備份損毀但 fallback 成功時仍會通知，並回報真實 RPO | ✅ | notify / verify 單元測試 |
| 6 | 匿名者無法讀取資料；viewer 無法寫入或偽造報告 | ✅ | RBAC 端點矩陣測試、`make e2e` |
| 7 | 每次觸發、靜音、登入都記錄到具名使用者，可匯出 CSV | ✅ | enterprise 測試、`make e2e` |
| 8 | Worker 在演練途中當掉，任務會在約 75 秒內重派並由其他 worker 完成 | ✅ | `make e2e`（`kill -9`） |
| 9 | Control Plane 重啟後，演練歷史與操作紀錄依保留期間完整保留 | ✅ | `TestHistoryRetentionPersistsAcrossRestart` |
| 10 | Web 金鑰不存放在 localStorage 或網址；暴力破解會被限流 | ✅ | session / 限流測試、瀏覽器實測 |
| 11 | daemon 依每個 target 的 cron 或 interval 排程 | ✅ | schedule / daemon 單元測試 |
| 12 | SSO / OIDC 登入 | ❌ | 尚未實作 |
| 13 | Control Plane 多節點高可用（共用資料庫） | ❌ | 尚未實作；目前為單節點 JSON 狀態檔 |
