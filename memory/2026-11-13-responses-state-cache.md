# Responses 状态缓存故障

- **日期**：2026-11-13
- **现象**：线上 `/v1/responses` 返回 409，消息为 `Responses state cache unavailable`。
- **根因**：`service/responses_state_store.go` 使用 `MGET` 读取多个 Responses 状态 key；迁移后的 ElastiCache Serverless Redis 开启 Cluster（`cluster_enabled:1`），这些 key 不在同一 slot，Redis 返回 `CROSSSLOT Keys in request don't hash to the same slot`。
- **修复**：改为逐 key `GET` 和 `EXPIRE`，避免跨 slot 多 key 命令；增加两个 key 的 Redis 回归测试。
- **验证**：`go build ./...`、`go vet ./...`、`go test ./...` 通过；`jsy-1`、`jsy-2` 已运行 `2364a778` hotfix 镜像，健康检查 200，容器启动后错误数为 0。
