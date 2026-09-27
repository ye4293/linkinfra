# TypeSafe System One 接入

## 背景与目标

按用户要求增加 `POST /v1/systemone`，兼容 TypeSafe 原生请求和响应，并纳入 LinkInfra 鉴权、渠道路由、重试、计费和日志。用户已授权实现本次接入。

## 方案设计

- 在 `router/relay-router.go`、`relay/constant/relay_mode.go`、`controller/relay.go` 注册独立协议模式，复用现有 Relay 生命周期。
- 新增 `relay/controller/systemone.go`，保留 `state`、`questions` 以及扩展字段；只按渠道映射替换模型名。使用渠道保存的上游密钥和地址，支持自定义渠道（类型 8，provider 为 typesafe）。
- 将 `usage.input_tokens/output_tokens` 转为通用 Usage，复用预扣与结算、折扣、消费日志、渠道与用户累计统计。失败返还预扣；缺失或非法 usage 不当作成功消费。
- 在 `common/model-ratio.go` 登记 `jev-latest`、`jev-preview`、`jev-1.13.0`：官方输入价格 $0.042/百万 tokens，输出免费；管理员仍可覆盖价格。
- 后台渠道测试识别 `jev-*` 或 `typesafe` provider，使用原生 System One 请求；请求同时纳入来源监控采集。
- 增加接入文档和回归测试，不将用户提供的上游密钥写入源码或提交。

## 影响范围

新增一个协议入口，现有聊天等接口行为不变，无数据库 schema 迁移。上线需要部署新版本并创建启用的 TypeSafe 渠道。

## 验证方式

- HTTP 模拟上游验证请求透传、模型映射、上游鉴权、地址规范化和原生响应。
- 内存数据库验证实际输入/输出用量、输出免费、折扣、固定价格、消费日志、余额和失败退款。
- 验证无效请求、上游错误、缺失 usage、重试与未认证请求。
- 使用提供的密钥进行最小上游连通性测试；执行相关包测试，以及隔离源码目录的完整 `go build ./...`、`go vet ./...`。
