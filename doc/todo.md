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
