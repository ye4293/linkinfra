# Kimi / Grok Claude Code 调用修复

## 背景与目标
使用本机 CC Switch linkinfra Claude 配置复现：kimi-k3 返回上游 `/v1/messages` 404；grok-4.7 基础请求成功，Claude Code 请求因 messages 内 system 角色返回 400。

## 方案设计
- Moonshot 显式实现 DoRequest，使用外层适配器，修复内嵌 OpenAI 接收者绕过 URL/header 覆盖；规范化 root、/v1、/anthropic 基址并保留查询参数。
- xAI 仅在 Claude 协议中把 messages 的 system 内容追加到顶层 system；保留文本块元数据、其他角色及未知字段。该兼容转换会将中途系统指令应用于整次推理，xAI 无法表达原协议的消息位置语义。
- 增加实际 HTTP 请求分派及角色兼容回归测试。

## 影响范围
限定 Moonshot 和 xAI 适配器；无数据库迁移，不更换 CC Switch 的网关或凭据。线上恢复需要部署后端修复。

## 验证方式
官方文档与直接请求对照；定向 Go 测试、go build ./...、go vet ./...；通过实际适配器转发 Claude Code 请求至官方接口验证。

## 官方依据
- https://platform.kimi.com/docs/guide/claude-code-kimi
- https://api.x.ai/api-docs/openapi.json

## 验证结果
Moonshot/xAI/relay controller/OpenAI 定向测试、完整 build/vet 通过。真实 Claude Code 经本地编译的修复适配器调用官方端点，两个模型均退出 0、is_error=false、返回 OK。未部署线上，CC Switch 地址和密钥未改动。

发布前复核：全量 `go test ./...`、`go build ./...`、`go vet ./...` 通过，发布版本 `v0.1.36`。

## 补充修复：xAI 工具 required 兼容
实际 Claude Code 默认工具集含 CronList、EnterWorktree 等省略 input_schema.required 的工具。最小请求实测：省略/null 均返回 400 `/required: null is not of type array`，显式 `[]` 返回 200。xAI 官方 OpenAPI 虽将此字段列为可选且可为 null，实际接口行为与文档不一致。

仅在 xAI Claude DoRequest 内补工具根 input_schema 的 required 空数组；不改嵌套 schema、默认值、有效必填约束、未知字段或其他协议。补齐 HTTP 分派回归测试，再用默认工具集及实际 Read 工具调用验证，执行全量 test/build/vet 后按已授权流程推送补丁 tag。

### 上游对照与补充验证
- xAI 官方 `/v1/messages` 已列为 deprecated：https://docs.x.ai/developers/rest-api-reference/inference/legacy 。接口定义与 required 的实际行为存在差异，使用最小 HTTP 请求核实。
- new-api 最新 release v1.0.0-rc.40 与 main d04c118c8803f49e0c9bab74dcf5b5efeab9464a：xAI 专用适配器 ConvertClaudeRequest 返回 not available；OpenAI 通用渠道则通过 relaykit 转为 Chat Completions，并将 input_schema 赋给 function.parameters；Claude 原生渠道保留 Messages 协议。透传选项会跳过转换，不是此错误的修复。
- 源码：https://github.com/QuantumNous/new-api/blob/d04c118c8803f49e0c9bab74dcf5b5efeab9464a/relay/channel/xai/adaptor.go
- 全部默认工具定义原请求返回 400，补各根 required 后返回 200。实际 Claude Code 经本地修复适配器和 CC Switch linkinfra 网关，以默认工具列表完成 Read 调用、工具结果回传，返回 LINKINFRA_TOOL_CHECK_OK。
- 全量 go test ./...、go build ./...、go vet ./... 均通过。

默认工具集 CLI 的独立 OK 请求首次超时，限制验证输出上限为 1024 后复测退出 0、is_error=false、返回 OK；未出现 Schema 400。Read 工具调用用例此前已通过。发布补丁版本 v0.1.37。
