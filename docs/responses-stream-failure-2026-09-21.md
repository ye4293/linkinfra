# Responses 解密失败与 0 token 记录排查

## 日志证据

分析用户提供的 Docker JSON 日志（2026-09-21 17:34 至 18:00）：

- 启动日志显示 `v0.1.32`。
- 17:55:27 请求 `202609211755272870381433559869` 选择渠道 15、key index 0，17:55:31 正常完成，输入 26296、输出 182 token。
- 17:55:32 请求 `2026092117553271694394071747902` 选择渠道 14、key index 0；两个渠道的 Azure 资源域名、脱敏密钥不同。该请求收到历史 reasoning 无法解密的 HTTP 400，只有一次尝试。
- 日志未保存被拒绝 reasoning item 的原始输出或密文来源索引，不能单凭时间相邻完全证明其生成来源；但可以确认期间存在跨 Azure 资源路由，不能据此认定“同一渠道自己生成的密文也不兼容”。
- 共有 34 条输入/输出均为 0 的消费日志，全部 `xResponseID` 为空。
- 17:42:25 渠道 14 的 Chat Completions 模型测试明确返回 429；这些 0 token 的 Responses 请求没有保留 SSE 错误载荷，无法追溯认定它们全部属于 429。

## 代码修复

此前流处理仅在 `response.completed` 读取用量，没有把 `response.failed`、`error` 或没有终止事件的 EOF 作为失败。它们会落入消费记录及亲和成功回写流程。

现在普通 JSON 响应和 SSE 都检测协议内错误，保留错误码和消息；明确的限流码或数字 429 映射为 429，未知上游错误映射为 502。缺少完成/正常 incomplete 终止事件的流返回 `responses_stream_incomplete`，不根据 token 数量猜测限流。

首个输出前发现错误，由外层按原 Provider 重试策略处理或返回对应 HTTP 状态。已经输出的流保留原始失败事件，不重放请求；HTTP 头已经发送时仍为 200，但内部错误记录保存业务失败状态，不再写成功消费记录。异常 EOF 会补充明确的 SSE 错误事件。

`response.incomplete`（例如达到输出上限）保留上游提供的真实用量。合法的完成响应即使为 0 token 也不按失败处理，不批量删除已有日志。

## 配置与验证

对尚未验证密文兼容的 Azure 资源使用不同 Provider 标识，参见 [渠道 Provider](channel-provider.md)。修改分组后从新会话验证；旧状态索引不随配置自动重写。

回归覆盖 HTTP 200 中的 JSON/SSE 限流、流开始前后失败、数字 429、加密历史错误、未知错误、空流、只有 `[DONE]`、截断流、合法 0 token 完成及 incomplete 用量。

审查补充：具体错误码优先于通用错误类型，避免 `rate_limit_exceeded` 或 `invalid_api_key` 与 `invalid_request_error` 同时出现时错误地映射为 400；显式 HTTP 错误状态仍优先。

2026-09-30 补充：流已开始输出后的中止事件改为规范的 `response.failed`（带嵌套 `response.error`），不再用裸 `{"type":"error"}`。Codex 只把 `response.failed` 当致命错误，`error` 事件除 flex 不可用外会被丢弃，导致客户端只报 `stream closed before response.completed` 并反复重试。已用本地 Codex CLI 0.159.0 与无余额 OpenAI key 实测对照：修复前重试 5 次后仍报 `stream closed before response.completed`；修复后首轮即报 `Quota exceeded`，不再重试。

官方协议参考：[Responses streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events)。本次未执行真实 Azure 联调或生产部署。

2026-09-30 二次验收：补齐上游已有 `response.failed` 的数字错误码及空错误对象兼容。畸形失败事件在发送前规范化，不先透出错误事件再追加第二个终止事件；合法事件保持原样。具体结果见 [修复与发布前验收](responses-compat-release-2026-09-30.md)。

ping 默认关闭，可由后台选项启用。Responses 等待上游 HTTP 头期间禁用 ping，因此首个输出前 HTTP 429 仍能按原策略重试并返回真实状态。收到上游 200 后可发送 ping；之后失败时 HTTP 已固定为 200，通过 `response.failed` 传递错误并记录内部业务状态。真实 Codex CLI 已验证先发 ping 后收到官方无余额错误仍可立即结束，无重连和扣费。

`Upstream Responses stream ended without a completed or incomplete response event` 表示未收到正常终止事件，内部错误码为 `responses_stream_incomplete`，状态 502。上游断流、超时、空流或仅 `[DONE]` 均可能触发；它不是限流的证据，也不代表 ping 一定造成了断流。修复保证异常被明确报告，不会将真实不完整响应伪造成成功。
