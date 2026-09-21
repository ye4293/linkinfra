# 渠道 Provider

渠道编辑页新增可选 Provider，例如 `openai`、`azure`。请为资源互通的渠道填写相同值，匹配时忽略首尾空格及大小写。

配置保存在渠道现有 `config` JSON 中，例如：

```json
{"provider":"azure","api_version":"2025-04-01-preview"}
```

更新 API 时保留 config 的其他已有字段。无需数据库迁移。

## 行为
- 首次选渠仍使用原来的权限、模型、优先级、权重和亲和规则。
- 首次渠道配置了 Provider：本次请求的失败重试只选择相同 Provider 的可用渠道；不同 Provider 或未配置 Provider 的渠道均不会被选中。
- 同 Provider 候选耗尽后返回错误，不跨 Provider，不重新循环已失败渠道，也不使用最后渠道兜底。
- 首次渠道未配置 Provider：保留原有重试行为，包括跨渠道重试。要启用隔离，应配置相关所有渠道。
- 保留原有错误重试条件、重试次数和指定渠道限制。

本功能不修改 thinking、compaction、工具调用或工具结果。不增加跨请求的会话绑定，下一轮对话的首次选渠仍可能使用其他 Provider。多 key 渠道内的密钥也应属于同一个 Provider。
