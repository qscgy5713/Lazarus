# 工作日誌 (Worklog)

## 2026-10-07 - 企業級 RBAC 鑑權與分散式 Worker 調度架構實作 (補齊最後 8% 差距)

### 做什麼 (What)
1. **RBAC 與 Token 鑑權體系實作**：
   - 劃分 `RoleAdmin`、`RoleViewer`、`RoleAnonymous`，支援 `AdminKey` 與 `ViewerKey` 配置（若未配置維持向下相容開放）。
   - 暴露 `/api/v1/auth/me` 與 `/api/v1/auth/login` 端點。
   - 對敏感動作（手動觸發演練 `trigger`、維護靜音 `mute`）嚴格校驗管理員權限，非 Admin 操作自動攔截並回傳 401/403。
   - Web 控制台實作 Token 本地存儲、即時身分 Badge、Token 解鎖彈窗與操作攔截提示。
2. **分散式邊緣 Worker 調度體系實作**：
   - 控制面擴充 Worker 註冊表，支援 `/api/v1/workers/register`、`/api/v1/workers/heartbeat`、`/api/v1/workers/poll` 與 `/api/v1/workers` 清單。
   - 支援邊緣節點動態心跳維護、60 秒無心跳自動標記為 `offline`。
   - 實作標籤匹配任務認領機制 (`ClaimPendingTarget`)，支援多團隊、多環境專屬邊緣節點調度。
   - Prometheus 導出企業級 Worker SRE 指標：`lazarus_workers_online`、`lazarus_workers_total`。
   - Web 控制台頂部導覽列整合「👷 Workers」拓撲彈窗，即時展示在線/忙碌狀態與運行任務。
3. **Lazarus CLI 分散式 Worker 常駐模式**：
   - 在 `cmd/lazarus/main.go` 實作 `--worker`、`--control-plane`、`--worker-id`、`--worker-token` 旗標與背景調度引擎。
   - Worker 啟動後自動註冊、定時發送心跳、輪詢認領待執行的演練任務，執行後回報結果並復原在線狀態；支援優雅平滑關閉。
4. **測試與工程品質**：
   - 在 `internal/server/server_test.go` 新增 `TestAuthRBAC` 與 `TestWorkersLifecycleAndPolling`。
   - 全專案 23 個套件測試與 `go vet` 靜態分析全部通過。

### 為什麼 (Why)
- 原先 Lazarus 產品完整度已達 92%，但在商業企業級場景中缺少兩大核心能力：多租戶/多角色操作安全審計（防止誤觸 trigger 或破壞演練排程）以及跨 VPC/混合雲的邊緣演練節點集中調度。補齊此兩項功能後，Lazarus 具備商用企業級完整能力。

### 遇到的問題與解法 (Issues & Solutions)
1. **問題**：若強制要求 Token，將破壞既有開發者本機環境與 CI/CD 流程的體驗。
   - **解法**：設計向下相容機制，當 Control Plane 未配置金鑰時，預設授予 `RoleAdmin`，無需額外配置即可無縫升級。
2. **問題**：Worker 輪詢時可能有多台 Runner 同時競爭同一個待執行的目標。
   - **解法**：在 `Store.ClaimPendingTarget` 中加讀寫鎖 (`sync.Mutex`)，在找到 `TriggerPending` 目標的當下立即重置為 `false`，並即時指派給認領的 Worker，徹底杜絕重複認領。
3. **問題**：單元測試中 `handleTriggerTarget` 回應 `StatusAccepted` (202)，原有測試誤斷言為 200。
   - **解法**：修正測試斷言為符合 RESTful 非同步任務規範的 `http.StatusAccepted` (202)。
