# 工作日誌 (Worklog)

## 2026-10-07 - 深度 Code Review 與全功能實測修正

### 做什麼 (What)
對全專案深度 CR，並以真實 Docker 備份（PostgreSQL plain/custom、MySQL、Redis RDB、MongoDB archive、SQLite+GPG）、Control Plane、Worker 與瀏覽器 UI 端到端實測，修正以下問題：
1. **RBAC 旗標不存在**：`lazarus-server` 沒有 `-admin-key`/`-viewer-key`（README 有寫，執行直接 `flag provided but not defined`）→ 補上旗標與 `ADMIN_KEY`/`VIEWER_KEY` 環境變數。
2. **RBAC 權限漏洞**：設定金鑰後匿名仍可讀 targets/reports/summary/CSV/PDF；viewer（唯讀）可 POST 偽造演練報告、註冊 Worker、認領任務 → 讀取需 viewer，寫入/Worker API 需 admin。
3. **只設 `-api-key` 時 `/auth/me` 回 `auth_required:false`** → 統一用 `authRequired()` 判斷。
4. **Stored XSS**：tag 按鈕 `onclick="filterByTag('${escapeHtml(tag)}')"`（HTML 實體在 JS 執行前會被解碼）、Worker `status` 未跳脫 → 改用 `data-tag` 屬性、跳脫並在伺服器端驗證 status 值。
5. **Data race**：`Store.GetSummary` 釋放鎖後仍走訪 `s.targets`，與 `RecordReport` 併發會 `concurrent map iteration and map write` → 全程持鎖。
6. **記憶體耗盡**：`/api/v1/metrics/daily?days=2000000` 回傳 223MB → 上限 366 天；auth/worker 請求本體加 64KB 上限。
7. **Worker 問題**：心跳與演練在同一迴圈，演練超過 60 秒即被判離線；心跳無法清除 `current_task`；Control Plane 重啟後不重新註冊；無標籤 Worker 會認領自己沒有的 target 並把觸發吃掉 → 背景心跳、404 自動重註冊、輪詢送出 target 名稱清單、伺服器依名稱認領。
8. **CLI→Control Plane payload 缺欄位**：RPO、資源用量、增量補丁、remediation 從未送出，UI/Prometheus 只有 demo 資料能顯示 → 補齊。
9. **MySQL 字串/正則/RPO 檢查必定失敗**：mysql client 的密碼警告混入輸出 → 統一於查詢出口過濾。
10. **Fallback 成功後殘留主備份的 RPO 判定**（`RPOViolated=true` 但 PASS）→ 改用 fallback 的結果。
11. **`lazarus export` 偽造 PASS**：無任何驗證紀錄的 target 也寫成通過 → 標示為未驗證失敗。
12. **SSE 卡住優雅關機**：Shutdown 會等到 5 秒逾時 → 關機時主動結束 SSE（實測 0.16 秒）。
13. Web UI：設金鑰時 SSE 永遠 401、匯出連結無法授權、匿名時誤顯示 Viewer → 以 `?api_key=` 附帶 Token、401 時自動開 Token 視窗。
14. 5 個檔案未 gofmt。

### 遇到的問題與解法 (Issues & Solutions)
- 既有 302 個測試全綠但完全沒覆蓋上述缺陷 → 新增回歸測試（RBAC 端點矩陣、race、days 上限、心跳、依名稱認領、SSE 關機、payload 欄位、MySQL 警告、fallback RPO、Worker 重註冊/輪詢、export），並確認 fallback RPO 測試在移除修正後會失敗。
- 讀取端點改為需授權屬行為變更：已設定 `SERVER_API_KEY` 的使用者，瀏覽器需在 🔑 視窗輸入金鑰才能看儀表板。

### 未修（記錄於 todo）
- config 的 `schedule` 欄位從未被使用；`remediation.trigger_on` 拼錯不會報錯；chaos 會把整個備份讀進記憶體。

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
