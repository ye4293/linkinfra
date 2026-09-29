# Responses 本地兼容性验收（2026-09-30）

> 以下是首次修复前的验收记录。两个缺口已修复，最终测试和真实 Codex 联调结果见 [发布前验收报告](responses-compat-release-2026-09-30.md)。

结论：文档所述主要修复的现有测试通过，但补充测试发现上游失败事件透传存在兼容性缺口，不能确认已经全部兼容。本轮仅测试和记录，没有修改业务源码、提交或部署。

## 验收对象

- 阅读 `docs/CHANGELOG.md` 中 Responses / Provider 相关记录及 `docs/responses-stream-failure-2026-09-21.md`。
- 基线提交：`04d7e96`，包含工作区尚未提交的四个 Responses Go 文件修改及两份文档修改。
- 环境：Windows amd64、Go 1.24.5、CGO_ENABLED=1。
- 全量检查使用 Git 跟踪文件的当前工作区内容副本，包含未提交修改，排除 `output/` 旧草稿和本地业务数据。副本与原工作区所有现存跟踪文件的 SHA-256 比较无差异。
- 隔离副本：`C:\Users\brows\AppData\Local\Temp\linkinfra-responses-audit-20260930-020020`。

## 执行结果

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout=180s` | 通过，27 个有测试的包 |
| `go build ./...` | 通过，退出码 0 |
| `go vet ./...` | 通过，退出码 0 |
| `go test -race ./service ./relay/controller ./controller ./model ./middleware -run 'TestResponses\|TestThinking\|Test.*Provider\|Test.*Retry' -count=1 -timeout=180s` | 五个包通过 |
| 新增隔离验收 `go test ./relay/controller -run '^TestResponsesAudit' -v -count=1 -timeout=60s` | 2 个子用例通过，2 个失败 |

构建输出包含第三方 `go-sqlite3` C 编译警告，未造成命令失败。现有全量测试先于新增验收用例执行；不能将“现有测试通过”解释为“补充验收通过”。

现有测试验证了 JSON/SSE 错误识别、流开始前后的错误处理、裸 error 补发终止事件、异常 EOF、合法 0 token 完成、incomplete 用量、状态缓存故障、reasoning/compaction 来源绑定、用户隔离、密钥轮换、Provider 约束和 Azure compact URL。

## 发现：上游 response.failed 的格式校验不足

位置：`relay/controller/responses_error.go:17` 的 `responsesTerminalFailure`，以及 `relay/controller/opeai_response.go:489` 的调用处。

当前只要事件类型为 `response.failed` 且 `response.error` 是对象，就认为它已是可用的终止事件并原样透传，不再进入字符串错误码转换及兜底逻辑。

在已输出 `response.in_progress` 后输入以下事件：

```json
{"type":"response.failed","response":{"status":"failed","error":{"code":429,"message":"Too many requests"}}}
```

实际下游仍为数字 `code: 429`，按客户端字符串错误码契约解析实际 SSE 得到：

```text
json: cannot unmarshal number into Go struct field .response.error.code of type string
```

同样，`{"type":"response.failed","response":{"status":"failed","error":{}}}` 会原样输出空错误对象，缺少错误码和消息，未使用已生成的内部 `responses_failed` 兜底信息。

对照用例中，裸 `error` 携带数字 429 可正确补出字符串错误码；`response.failed` 携带字符串错误码也通过。因此问题具体在上游已有失败事件的透传分支，不是所有错误路径都失败。

建议：仅透传字段类型及内容有效的失败事件；异常形状应输出规范化后的单个终止失败事件，保留错误信息并补齐兜底值。需要覆盖实际下游 SSE，不能只测试 `responsesFailureEvent` 辅助函数。

## 验证边界与证据

- 本轮用本地模拟上游及隔离测试库执行测试，没有执行真实 OpenAI/Azure 联调，也没有重新运行 Codex CLI。上述客户端解析失败由 Go 字符串字段解码契约复现，不冒充真实 Codex CLI 实测结果。
- 文档中的历史 Codex CLI 实测结果不属于本轮证据。真实 Azure reasoning 密文兼容仍需按资源、Provider 配置和新会话联调确认。
- 本轮读取了 [OpenAI 官方 Responses streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events)，核对 `response.failed` 及嵌套 `response.error` 示例。该文档不能单独证明所有客户端的运行行为。
- 失败后不计费、不重放的现有分支已作源码核对；本轮未新增针对 Responses 的完整鉴权、数据库计费、客户端重试端到端测试。
- 原始日志及复现用例保存在 `output/responses-compat-audit-2026-09-30/`：`test-all.log`、`build.log`、`vet.log`、`race.log`、`audit-boundary.log`、`responses_compat_audit_test.go.txt`。用例副本以 `.txt` 保存，避免被仓库 `go test ./...` 当作独立包编译。
- 如需复现，可直接在上述隔离副本运行新增验收命令；或将 `.go.txt` 用例复制到另一个源码副本的 `relay/controller/responses_compat_audit_test.go` 后执行。
