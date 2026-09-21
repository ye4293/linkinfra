# 渠道 Provider

渠道编辑页新增可选 Provider，例如 `openai`、`azure`。请为资源互通的渠道填写相同值，匹配时忽略首尾空格及大小写。

配置保存在渠道现有 `config` JSON 中，例如：

```json
{"provider":"azure","api_version":"2025-04-01-preview"}
```

更新 API 时保留 config 的其他已有字段。无需数据库迁移。

## 行为
- 不带上游状态的首次选渠使用原来的权限、模型、优先级、权重和亲和规则。
- 首次渠道配置了 Provider：本次请求的失败重试只选择相同 Provider 的可用渠道；不同 Provider 或未配置 Provider 的渠道均不会被选中。
- 同 Provider 候选耗尽后返回错误，不跨 Provider，不重新循环已失败渠道，也不使用最后渠道兜底。
- 首次渠道未配置 Provider：保留原有重试行为，包括跨渠道重试。要启用隔离，应配置相关所有渠道。
- 保留原有错误重试条件、重试次数和指定渠道限制。

多 key 渠道内的密钥也应属于同一个 Provider。资源是否互通由管理员确认，Provider 名称本身不构成平台互通保证。

## 携带 thinking 的连续对话

Responses 和 `/v1/responses/compact` 自动按实际返回状态绑定来源，无需客户端新增会话 ID：

- 后端记录返回的 reasoning/compaction 密文指纹、item ID、response ID、conversation ID 的来源；下一轮携带状态时，首次选渠和失败重试都限定在原 Provider。
- `previous_response_id`、`item_reference`、`conversation` 以及没有密文的 reasoning/compaction ID 是服务端资源引用，额外固定原渠道/key；没有配置 Provider 的状态也固定原渠道/key。原 key 禁用、更换或不再可用时明确失败。
- 不清理 thinking/compaction，不重写工具调用或结果。模型映射只修改 model，保留扩展字段和显式 false/0。
- 普通响应和流式事件的状态在转发给客户端前写入索引。流开始后不透明重放；本地状态缓存失败不会被判定为上游渠道故障。
- 多用户的状态索引隔离。索引只保存 SHA-256 指纹与 provider、渠道/key 元信息，不保存正文、密文或密钥明文。

索引默认保留 7 天，读取会续期。启用 Redis 时使用共享索引；Redis 故障时明确失败，不随机换 provider。未启用 Redis 时使用最多 100000 项的进程内缓存，适合单实例；重启/淘汰后来源无法恢复，多实例应配置共享 Redis。

未知、过期或混合不同 Provider 的状态会返回明确错误，不删除上下文后继续请求。升级前已有的加密历史没有新索引，可显式传入原来源，例如 `X-Linkinfra-Provider: openai`；该头不能覆盖已知且冲突的来源，也不能定位未知的服务端引用。未知服务端引用需恢复完整历史或重新开始会话。此头同样受模型和用户组权限约束。
