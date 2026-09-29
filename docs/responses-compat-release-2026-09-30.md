# Responses 修复与发布前验收

本次 Responses 失败事件兼容修复已完成，最终代码审查未发现本次改动的阻断问题，本地发布检查通过。可将本次修复提交纳入后端 tag。此结论覆盖已测试的错误处理、ping、重试、日志和计费路径，不代表真实 Azure 跨资源密文已经验证兼容。

## 修改与审查

- 上游已有 `response.failed` 时检查错误码和消息类型及内容。数字错误码转为字符串；空/null/缺失错误对象及空白字段补齐兜底。原位修正后只发送一个终止事件，避免客户端先遇到畸形终止事件。
- 合法事件保持字节级原样；修正事件保留 sequence_number、response id、响应元数据、错误扩展字段及大整数精度。
- 裸 error、EOF、缓存故障仍输出可识别的 `response.failed`；缓存失败不泄露未成功绑定的 response id。
- 完整路由 race 检查复现了 Responses 渠道错误任务在请求返回后读取全局配置的问题；将这三个调用改为请求内完成，与现有 System One 生命周期处理一致。
- 性能审查：额外 JSON 解析与编码仅发生在畸形终止失败事件，按单个事件大小线性处理，没有增加正常流的缓存、每 token 数据库访问或循环重试。渠道错误检查仍是原有操作，移至请求内会在失败路径等待其完成，换取明确的生命周期及重试状态顺序；未进行负载压测。

## 最终本地验证

Windows amd64，Go 1.24.5，CGO 开启。隔离副本包含当前跟踪源码和两个新增测试文件，所有 Go 文件与工作区 SHA-256 对照无差异；排除旧 `output/` 草稿。副本位置：`C:\Users\brows\AppData\Local\Temp\linkinfra-responses-final-20260930-021151`。

| 验证 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout=180s` | 27 个有测试的包通过 |
| `go build ./...` | 退出码 0 |
| `go vet ./...` | 退出码 0 |
| router、relay/controller、service、controller、model、middleware 的 Responses / Thinking / Provider / Retry 定向 race | 六包通过 |
| 实际 SSE 错误字段解析 | 10 个边界子用例通过 |
| 完整 HTTP 路由生命周期 | 数字错误、空错误、ping 后 error、ping 后 EOF、HTTP 429 五个场景通过 |
| `git diff --check` | 通过 |

生命周期测试使用生产鉴权、选渠和 RelayResponse，本地 HTTP 上游与内存 SQLite。配置两个同 Provider 渠道及两次重试：流开始后实际仅一次上游请求；HTTP 429 在任何输出前发生时正确尝试两个渠道，最终仍返回 HTTP 429。失败后用户和令牌额度均保持 1,000,000，只有一条错误日志，没有消费日志。

第三方 go-sqlite3 的 C 编译警告不影响以上退出码。真实上游用例默认跳过，必须显式提供测试环境变量。

## 真实 Codex CLI 与 OpenAI 联调

使用用户授权的无余额官方 key、Codex CLI 0.159.0、`gpt-5.1`、`wire_api=responses`。本地服务由测试进程启动实际生产路由，使用内存数据库中的隔离用户、令牌和 OpenAI 渠道；不是生产部署，也不修改常用本地业务库。

| 场景 | Codex 结果 | 上游请求数 | 耗时 | 扣费 / 消费日志 |
| --- | --- | --- | --- | --- |
| 正常读取真实上游响应 | `Quota exceeded. Check your plan and billing details.` | 1 | 1.92s | 0 / 0 |
| 收到上游 200 后，等实际 ping 发出再读取原始响应体 | 同上 | 1 | 2.42s | 0 / 0 |

第二轮仅通过测试传输层延后读取，不改写上游内容。实际下游事件为 `ping → response.created → response.in_progress → error → response.failed`。两轮均没有 `Reconnecting` 或 `stream closed before response.completed`；内部错误为 `credit_balance_exhausted`、状态 429，每轮一条错误日志。

首次 CLI 启动尝试没有产生上游请求并超时，因此不计入成功证据。修正测试环境的回环代理绕过与继承的 daemon/会话设置后完成上述验证。凭据仅经测试进程环境传入，子 Codex 只收到本地网关令牌；源码、文档和验收日志检查未发现完整官方 key。

## ping 与 502 的结论

ping 默认关闭，但可由后台选项开启。Responses 等待上游 HTTP 头阶段显式禁用 ping，避免提前锁死状态码。上游返回 200 后才允许流式保活。

如果已输出 ping 或内容，HTTP 200 无法改变；之后的失败通过 SSE `response.failed` 结束，内部日志仍记录 429/502 等业务状态。外层 RelayResponse 看到已经写出就不再重试或拼接 JSON，终止事件由流处理层负责。

`responses_stream_incomplete` / `Upstream Responses stream ended without a completed or incomplete response event` 是缺少正常终止事件的保护错误，记录为 502。本地测试确认 ping 后直接 EOF 会报告该错误、不重放、不扣费。这不能证明真实上游断流的根因，也不会消除网络或上游本身的故障。

## 证据与发布边界

日志位于 `output/responses-compat-audit-2026-09-30/`：`test-final.log`、`build-final.log`、`vet-final.log`、`race-final.log`、`lifecycle-final.log`、`codex-live-final.log`。首次问题复现和 race 失败日志保留用于对照。

无数据库迁移；未执行真实 Azure reasoning 密文联调、成功生成会话/工具调用的端到端测试或负载压测。用户随后授权发布 v0.1.40，发布与交接说明见 [v0.1.40](releases/v0.1.40-responses-compat.md)。
