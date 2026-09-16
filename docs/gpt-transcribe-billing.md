# GPT Transcribe 计费与定价

后端为 `linkinfra`，实际前端为 `linkinfra-web`。

## 设置价格

前端进入设置 → 模型定价 → 按时长计费，填写 `gpt-transcribe` 或其他转录模型，输入美元/分钟价格并保存；每行一个模型可批量设置。也可以从已配置模型、未设置模型列表直接进入配置。
后端内置 `gpt-transcribe` 单价为 **$0.0045/分钟**；编辑时显示当前配置，保存前应按实际业务价格核对。
填入 `0` 表示免费，移除 `gpt-transcribe` 的价格会阻止其转录请求，避免错误地按 token 计费。

配置存放在现有 Option 的 `AudioDurationPrices` 键中，值为 JSON 对象字符串：

```json
{"gpt-transcribe":0.0045}
```

前端通过 `/api/option` 读取配置，单个更新通过 `PUT /api/pricing/model`，批量更新通过前端 `PUT /api/pricing/batch` 代理到后端 `/api/pricing/models/batch`。
请求字段为 `model_name` 和 `duration_price_per_minute`；移除时使用 `remove_duration_price: true`，不可同时指定新价格。省略价格表示不修改，0 表示免费。批量请求使用 `{ "models": [...] }`。
后端在进程内锁中读取、合并并持久化所选模型，保留其他条目；完整批次先校验，写库成功后才更新运行价格。该锁不保证多实例同时修改配置的串行化；多实例应通过同一管理写入实例更新。旧 `/api/option` 仍支持完整替换，调用方需保留其他条目。
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

## 2026-09-16 补齐范围

管理端支持单个/批量设置和移除分钟价格；模型广场卡片、表格和详情显示美元/分钟，支持按时长筛选；首页目录接受时长模型。日志区分显式 0 秒与缺失时长，展示实际结算 quota。
部署顺序为后端先于前端。回归使用内存数据库和模拟上游，不代表真实渠道已逐个验收。

## 官方参考

价格和协议于 2026-09-14 核对：

- https://developers.openai.com/api/docs/models/gpt-transcribe
- https://developers.openai.com/api/reference/resources/audio/subresources/transcriptions/methods/create
