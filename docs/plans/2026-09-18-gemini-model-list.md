# Gemini 模型列表兼容

## 背景与目标
用户已授权参考本地 new-api 实现兼容。Gemini 渠道请求 `/v1beta/openai/models`，当前平台缺少该入口，平台级联时返回 404。

## 方案设计
- `router/relay-router.go`：增加经过 TokenAuth 的 OpenAI 兼容及 Gemini 原生模型列表入口，不经过渠道分配。
- `controller/model.go`：原生入口将现有模型目录转换为 Gemini 的 `models` / `name` / `displayName` 格式；兼容入口复用现有 OpenAI 列表。
- `controller/channel.go`、`controller/channel_upstream_update.go`：共用获取函数，仅 Gemini 的兼容入口返回 404 时回退到 `/v1/models`，保留路径前缀。
- 增加回归测试，覆盖路由、鉴权、响应格式、回退条件和获取入口。

## 影响范围
不调整现有模型目录来源及权限规则，不涉及数据库迁移。官方入口成功时维持单次请求，认证错误、限流和服务器错误不触发回退。

## 验证方式
使用内存数据库及 httptest 服务验证兼容行为，执行相关测试、完整 Go build 和 vet。
