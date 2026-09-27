# Messages 渠道统一检查

## 目标与范围
检查已经实现 Anthropic Messages 的 DeepSeek、百度千帆、GLM、阿里百炼、MiMo、Moonshot、MiniMax、xAI、Anthropic；AWS Bedrock、Vertex AI 另按其 SDK/云端转换链路检查。不能把通用 OpenAI 渠道的路径直传视为供应商已支持 Messages。

## 方案
新增通过生产 GetAdaptor 工厂发起真实本地 HTTP 请求的矩阵测试，检查 root/SDK base、查询参数、鉴权、beta/version、流式与非流式、原始工具/思考/缓存字段。若 GLM、阿里、Anthropic 的固定 URL 拼接造成重复前缀或丢查询参数，仅修相应 Messages 分支，避免改动其他协议和计费。

## 验证
先运行测试观察失败，再实施修复。重跑现有 DeepSeek/千帆/MiMo 等实际分派用例、AWS/Vertex 转换测试及全量 test/build/vet。本地 HTTP 测试只验证网关发送行为，不代表所有模型功能均获官方支持，不冒充线上实测。

## 检查结果

| 渠道 | 本地验证结论 |
|---|---|
| DeepSeek | 实际使用 OpenAI 适配器内的 DeepSeek 分支，URL/鉴权生效；无内嵌分派问题 |
| 百度千帆 | 显式外层 DoRequest，Messages 使用 x-api-key；通过 |
| MiMo | 显式外层 DoRequest；通过 |
| Moonshot / MiniMax | 此前修复的外层分派通过统一回归 |
| xAI | 独立适配器，现有 system/required 修复保留；通过 |
| GLM | 修复 SDK base 重复前缀、丢查询参数及 ActualAPIKey 为空时的回退 |
| 阿里百炼 | 修复 Messages SDK base 重复前缀与丢查询参数；其他协议不改 |
| Anthropic | 修复末尾斜杠、/v1 重复与原生 Messages 丢查询；OpenAI 转 Claude 仍固定 /v1/messages |
| AWS / Vertex | 独立 SDK/云端转换链路，无内嵌 OpenAI 分派；既有协议转换、模型映射、请求测试通过 |

新增 124 组真实本地 HTTP 用例，通过生产工厂覆盖 9 渠道、基址变体、流式/非流式、主/备用密钥，确认请求及响应内容未变化；请求体包含工具调用回传、思考、缓存和未知字段。新增 Anthropic 的 OpenAI 转换入口回归。修复前矩阵显示 GLM/阿里/Anthropic 失败，修复后全部通过。全量 go test ./...、go build ./...、go vet ./... 通过。

本次没有逐供应商在线推理测试，结论仅针对网关实现，不保证模型支持全部 Claude 功能。原生 OpenAI/Gemini/Tencent 等未声明 Messages 支持的适配器，不因通用路由接受 /v1/messages 就算已完成兼容。

## 官方参考
- DeepSeek：https://api-docs.deepseek.com/guides/anthropic_api/
- 百度：https://cloud.baidu.com/doc/qianfan-docs/s/6mh3e6gjp
- GLM：https://docs.bigmodel.cn/cn/guide/develop/claude
- 阿里：https://help.aliyun.com/zh/model-studio/anthropic-api-messages
