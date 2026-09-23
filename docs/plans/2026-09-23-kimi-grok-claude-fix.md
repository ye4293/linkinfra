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
