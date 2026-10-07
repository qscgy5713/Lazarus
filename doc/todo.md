# 待辦與進度清單 (TODO)

## 企業級分散式災難復原平台強化 (100% Milestone)
- [x] 資料模型層：在 `internal/server/models.go` 定義 `UserRole`、`AuthStatusResponse`、`WorkerStatus`、`WorkerRecord`、`WorkerPollResponse`
- [x] 存儲層：在 `internal/server/store.go` 擴充 `workers` 映射表、實作 `RegisterWorker`、`HeartbeatWorker`、`GetWorkers`、`ClaimPendingTarget`
- [x] 控制面伺服器：在 `internal/server/server.go` 擴充 `AdminKey` 與 `ViewerKey`、實作 RBAC 權限中介層、註冊 Auth 與 Worker 相關 API 路由
- [x] Prometheus 指標：在 `internal/server/metrics.go` 暴露 `lazarus_workers_online` 與 `lazarus_workers_total`
- [x] Web 控制台 UI：在 `internal/server/index.html` 加入「👷 Workers」與「🔑 Admin/Viewer」面板、實作 `authFetch`、Token 本地存儲、即時狀態渲染與權限攔截
- [x] CLI 分散式 Worker 模式：在 `cmd/lazarus/main.go` 實作 `--worker` 模式、註冊心跳與輪詢認領待執行的演練任務
- [x] 單元測試：在 `internal/server/server_test.go` 新增 `TestAuthRBAC` 與 `TestWorkersLifecycleAndPolling` 完整涵蓋
- [x] 驗證：全專案 23 個套件測試 (`go test -count=1 ./...`) 與靜態分析 (`go vet ./...`) 100% 通過
- [x] 文檔：更新 `README.md`、`lazarus.example.yml` 與 `doc/` 文件索引

## 深度 CR 修正 (2026-10-07)
- [x] `lazarus-server` 補上 `-admin-key` / `-viewer-key` 旗標
- [x] RBAC：讀取需 viewer；回報/Worker/觸發/靜音需 admin；`auth_required` 涵蓋舊版 `api-key`
- [x] 修 Web UI stored XSS（tag onclick、worker status）
- [x] 修 `Store.GetSummary` data race
- [x] `metrics/daily` days 上限與小型請求本體上限
- [x] Worker：背景心跳、重新註冊、依 target 名稱認領、清除 current_task
- [x] CLI→Control Plane payload 補齊 RPO / 資源 / 補丁 / remediation 欄位
- [x] MySQL 字串/正則/RPO 檢查過濾密碼警告
- [x] Fallback 成功時使用 fallback 的 RPO 判定
- [x] `lazarus export` 不再把未驗證 target 標為 PASS
- [x] SSE 不再阻塞優雅關機；UI SSE/匯出帶 Token
- [ ] config `schedule` 欄位實作 cron 排程或移除
- [ ] 驗證 `remediation.trigger_on` 只能是 `failure` / `critical_drift`
- [ ] chaos 改為串流複製，避免大型備份整檔載入記憶體
- [ ] UI 對 403 改用非阻塞提示取代 `alert()`
