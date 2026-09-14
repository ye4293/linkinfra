# GPT Transcribe 计费与定价

后端为 `linkinfra`，管理前端为 `ezlinkai-web-next`。

## 设置价格

前端进入系统设置 → 模型定价 → 按分钟计费，选择 `gpt-transcribe`，输入美元/分钟价格并保存。
参考价格为 **$0.0045/分钟**；前端提供填入参考价格的按钮，填写后仍需保存。
填入 `0` 表示免费，移除 `gpt-transcribe` 的价格会阻止其转录请求，避免错误地按 token 计费。

配置通过现有 `/api/option` 接口保存，键为 `AudioDurationPrices`，值为 JSON 对象字符串：

```json
{"gpt-transcribe":0.0045}
```

该对象同时包含所有按分钟计费的模型，修改时应保留其他条目。前端保存前重新读取最新配置并合并所选模型，使用一次请求保存。
后端内置 `gpt-transcribe` 的上述价格；已保存的 `AudioDurationPrices` 会覆盖内置配置。
按分钟价格优先于同名模型的旧倍率或按次价格，模型定价列表与模型广场均显示每分钟单价。

## 用量与结算

- 支持音频转录、翻译路由；不影响 TTS 和 Realtime。
- 请求的 `response_format` 必须是 `json`、`verbose_json` 或 `diarized_json`，也支持这些格式对应的流式请求。
- 优先读取上游 `usage.type=duration` 的 `usage.seconds`，显式零秒保持零费用。
- 上游只有顶层 `duration` 时使用该值；SSE 仅读取 `transcript.text.done` 结束事件中的累计用量，不累加片段。
- 缺少有效时长、未收到流式结束用量或上游失败时，退回预扣额度。不以 token 数量或文件大小估算时长。
- 以一分钟费用预扣，随后按实际用量补扣或退款；用户与令牌余额在同一数据库事务内更新，失败退款不会重复增加用户余额。
- 单价、额度换算系数和组合倍率在请求开始时固定。上游模型映射不改变原模型的价格或日志名称。
- 映射到 `gpt-transcribe` 的别名也必须按请求中的模型名称配置分钟价格，不能通过别名绕过时长计费。

```text
quota = round(seconds / 60 × price_per_minute × QuotaPerUnit × combined_ratio)
```

组合倍率沿用等级折扣、渠道折扣、用户渠道折扣的既有规则。计算使用十进制价格的精确有理数，最终只按内部额度取整一次。
默认 `QuotaPerUnit=500000`、倍率为 1 时，600 秒费用为 $0.045，即 22500 quota。
9 秒对应 337.5 quota，最终取整为 338 quota；显示金额因此可能与未取整值略有差异。

消费日志 `other` 记录 `billing_mode=duration`、`duration_price_per_minute`、`audio_duration_seconds`、`transcription_usage_source`、`group_ratio`。
`transcription_usage_source` 为 `upstream` 或 `missing`。时长不伪装成 token 数，日志的 token 计数为零；展开日志可查看秒数、分钟单价和结算公式。

## 官方参考

价格和协议于 2026-09-14 核对：

- https://developers.openai.com/api/docs/models/gpt-transcribe
- https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create
