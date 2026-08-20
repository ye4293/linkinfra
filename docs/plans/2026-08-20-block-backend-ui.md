# 屏蔽后端内嵌 UI，根路径改为返回 JSON

## 背景与目标

linkinfra 后端启动后，访问根路径 `/` 仍能看到内嵌的 React 管理控制台 UI（`web/default`），公网可直接访问。根因在 `router/main.go:28-40`：上游 one-api 的设计是"master 节点忽略 `FRONTEND_BASE_URL`、强制服务内嵌 UI；只有非 master 节点才 301 跳转到前端"。本部署是单 master + 独立 Next 前端（`NODE_TYPE=master`，compose 已配 `FRONTEND_BASE_URL`），这套上游逻辑把配好的 `FRONTEND_BASE_URL` 直接清空，导致 `SetWebRouter` 照常在 `/` 服务内嵌 UI，`NoRoute` 还把任意非 `/api`、`/v1` 路径回吐 index.html。

目标：后端只作为 API 服务暴露，根路径与非 API 路径返回一段纯 JSON（仿 `https://api.openai.com/` 根路径响应），不再泄露管理控制台 UI。`/api/*`、`/v1/*` 行为不变。

## 方案设计

1. `router/web-router.go` — `SetWebRouter` 改为只返回 JSON：删掉 `static.Serve("/")`、`router.Use(gzip/Cache/GlobalWebRateLimit)`、`indexPageData` 读取；`NoRoute` 对 `/v1`、`/api` 前缀调 `controller.RelayNotFound`（现状不动），其余路径返回 `c.JSON(200, gin.H{"message":"Welcome to the LinkInfra API!"})`；`buildFS` 保留以兼容调用链（`_ = buildFS`）。
2. `router/main.go` — `SetRouter` 简化：删掉 `FRONTEND_BASE_URL` 读取、master 忽略分支、301 跳转分支，直接 `SetWebRouter(router, buildFS)`。
3. `FRONTEND_BASE_URL` 废弃，同步 `.env.example` 与两个 docker-compose 的注释。
4. `docs/CHANGELOG.md` 追加记录。

## 影响范围

- `/api/*`、`/v1/*`、`/dashboard/*` 行为不变：gin v1.9.1 的 `router.Use` 只影响"调用之后注册的路由"，三者 handler 链在 `SetWebRouter` 之前已用 `router.Group(...).Use` 固化。
- `/api` 限流是路由级显式挂的（`router/api-router.go`），删 engine 级 `GlobalWebRateLimit` 不动它们；`middleware.Cache` 仅设响应头，与 API 无耦合。
- `FRONTEND_BASE_URL` 跳转分支只在 `router/main.go` 内，当前 master 部署本就没走跳转（被忽略逻辑清空），前端从未依赖后端 301 跳转，删除零影响。
- `buildFS` 无别处引用；保留参数最省事且零编译问题。

## 验证方式

1. `go build ./... && go vet ./...` 通过。
2. `curl -i`：
   - `GET /` → `200 {"message":"..."}`（非 HTML）。
   - `GET /console` → 200 JSON（不再回吐 SPA）。
   - `GET /api/status` 正常（健康检查不变）。
   - `GET /v1/chat/completions` 正常 relay/401（不变）。
   - `GET /api/notexist` → 404 OpenAI 风格 error JSON（`controller.RelayNotFound`）。
3. 前端 linkinfra-web 调后端 API 正常（回归）。
