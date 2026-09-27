# TypeSafe System One

LinkInfra 提供 `POST https://api.linkinfra.ai/v1/systemone`，请求与成功响应使用 [TypeSafe 原生协议](https://docs.typesafe.ai/api)。客户端使用 **LinkInfra 令牌**；TypeSafe 上游密钥保存在管理后台渠道中。

## 渠道配置

部署包含本次接入的后端后，在渠道管理中创建：

| 配置项 | 值 |
| --- | --- |
| 类型 | TypeSafe（`type: 50`） |
| 名称 | TypeSafe |
| 上游地址 | `https://api.typesafe.ai`（自动填入，留空也使用此默认值） |
| 密钥 | TypeSafe 提供的 `apikey_...` |
| 模型 | `jev-latest,jev-preview,jev-1.13.0` |
| 分组 | 实际调用用户所在分组 |
| provider | 可留空，按 TypeSafe 类型自动识别；需要覆盖时填写 `typesafe` |
| 测试模型 | `jev-latest` |
| 状态 | 启用 |

上游地址也支持以 `/v1` 或 `/v1/systemone` 结尾。渠道支持模型映射、多密钥、优先级、折扣和请求头覆盖。模型名为 `jev-*` 或 provider 为 `typesafe` 时，后台渠道测试自动使用 System One 原生请求。

后台选择 TypeSafe 时自动填入默认地址、三个 Jev 模型和测试模型；“获取上游模型”支持原生 `models[].name`。已有自定义渠道（type 8，手动配置官方地址和 provider）仍可继续使用。独立 TypeSafe 渠道的后台测试始终使用 System One，即使渠道配置了自定义模型别名。

上游密钥不应传给 LinkInfra 客户端，也不应写入源码、示例或日志。

## 请求示例

```bash
curl https://api.linkinfra.ai/v1/systemone \
  -H "Authorization: Bearer $LINKINFRA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "jev-latest",
    "state": "Help! My payouts have been failing for 3 days.",
    "questions": {
      "is_urgent": {
        "type": "noul",
        "instructions": "Does this convey urgency?"
      }
    }
  }'
```

支持 `noul`、`choice`、`score` 和结构化 `state`、`instructions`、`criteria`。同一请求可以包含多个问题。响应保留 `model`、`answers`、`usage` 及扩展字段，模型映射只替换发给上游的 `model`；不支持 `stream: true`。

## 计费和日志

默认价格来自 [TypeSafe 模型文档](https://docs.typesafe.ai/models)，核对日期 2026-09-27：

| 模型 | 输入 / 百万 tokens | 输出 / 百万 tokens | 输入倍率 | 输出倍率 |
| --- | ---: | ---: | ---: | ---: |
| `jev-latest` | $0.042 | $0 | 0.021 | 0 |
| `jev-preview` | $0.042 | $0 | 0.021 | 0 |
| `jev-1.13.0` | $0.042 | $0 | 0.021 | 0 |

管理员可使用现有价格配置覆盖默认值，也可配置按次计费。升级时已有价格覆盖会保留；别名指向新版后应重新核对官方价格。

最终用量采用上游 `usage.input_tokens` 和 `usage.output_tokens`。输出 token 免费但仍计入用量日志。输入按现有额度精度向上取整，例如无折扣时 1,000 输入 token 扣 21 quota。分组、渠道、用户渠道、模型折扣沿用现有规则；映射后的模型决定计费价格。

请求发送前检查并预扣用户和令牌额度，失败退回预扣；缺失或非法 usage 返回 502 并退款，不使用本地估算用量结算。上游 429/529 按现有重试配置处理，422 不重试。成功只结算一次，消费日志包含请求 ID、渠道、输入／输出 tokens、价格、折扣和重试记录；最终失败写入错误日志。错误响应沿用 LinkInfra 的 `error` 格式，并保留上游状态码及错误描述。

预扣、退款及最终余额和用量更新使用即时事务，不受全局批量更新设置影响。每次上游尝试开始时冻结价格与折扣，使用十进制精确计算避免浮点误差多扣 1 quota。结算数据库失败返回 500，回滚并尝试退款，禁止网关自动重试该请求；数据库持续不可用时退款失败会写入服务错误日志，需恢复后核对。零用量成功请求仍记录消费日志和请求次数。

答案必须覆盖全部问题并匹配类型和基本结果字段。非协议的 2xx（例如空 204）按 502 处理；非 JSON 错误保留可读的 HTTP 状态说明。最终失败透传 `Retry-After`，重试成功清除旧的重试提示。

## 本次验证

- 用户提供的上游密钥完成一次最小原生请求，HTTP 200，返回 `jev-1.13.0`，输入 278 / 输出 20 tokens。
- 自动化测试覆盖原生字段透传、结构化数据与大整数保真、模型映射、上游鉴权、价格和折扣、固定价格、令牌额度不足、错误退款、路由鉴权、529 重试成功后的单次结算、422 错误日志与后台渠道测试。
- 上游直连成功不代表线上已部署。需要将新后端部署至域名对应服务，并在该服务上创建启用的渠道，才能使用上述公开地址。
