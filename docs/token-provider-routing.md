# 令牌的来源路由与 provider 互通规则

用户可以在 API key（令牌）上决定请求使用哪些上游来源、先后顺序，以及永不使用哪些来源。管理员可以声明哪些 provider 之间可以互相重试。两项都是可选的：没有设置路由的令牌、没有配置互通规则的模型，选渠和重试与之前一致。

渠道上的 Provider 字段见 `docs/channel-provider.md`。

## 来源

**来源**是用户看到并选择的上游，与模型广场的 `source_key` 相同：

- 渠道填了 `config.provider`：来源就是 provider。
- 没填：来源是渠道类型的名称（例如 `anthropic`、`vertex ai`、`aws`、`azure`）。
- 统一小写，忽略首尾空格。

同一个来源可以包含多个渠道。`GET /api/token/sources`（登录用户）返回当前用户等级下可用的来源，每项含来源名 `name`、显示名 `display_name` 和可用模型 `models`。

## 令牌的来源路由

`tokens.provider_routing` 存一段 JSON，没有设置时为 NULL：

```json
{"order":["vertex","anthropic"],"only":true,"ignore":["aws"]}
```

| 字段 | 界面 | 含义 |
|---|---|---|
| `order` | Provider Selection | 选中的来源，顺序即使用的先后 |
| `only` | Only Selected Providers | 为 true 时只用 `order` 里的来源；为 false 时其余来源排在最后作后备 |
| `ignore` | Ignore Providers | 永不使用的来源 |

### 选渠规则

1. 候选渠道是用户等级和模型下启用的渠道，去掉本次请求已失败的、不在重试范围内的、令牌路由不允许的。
2. 令牌路由不允许的渠道：来源在 `ignore` 里；或 `only` 为 true 而来源不在 `order` 里。
3. 按 `order` 逐个来源尝试；`only` 为 false 时，其余来源合并为最后一层。当前来源没有可选渠道才进入下一个来源。首选和失败重试都按这个顺序。
4. 来源内部沿用管理员设置的优先级和权重。
5. `order` 为空时只有一层：除 `ignore` 外的所有来源，按管理员的优先级。

`ignore` 的优先级最高。被忽略的来源在首选、失败重试、互通规则扩大后的重试范围、后备层、亲和规则和 `X-Response-ID` 命中的渠道、count_tokens 里都不会被选中。

示例（Claude 有 `anthropic`、`vertex`、`aws` 三个来源，都写在一条互通规则里）：

| 令牌的设置 | 首选 | 失败后的重试 |
|---|---|---|
| 不设置 | 按管理员的优先级和权重 | 三个来源之间都可以 |
| `order=[vertex]`，`only=false` | vertex | 先 vertex，再到 anthropic、aws |
| `order=[vertex,anthropic]`，`only=true` | vertex | 先 vertex，再到 anthropic，不会到 aws |
| `order=[vertex]`，`only=true` | vertex | 只在 vertex 的渠道内，用完就报错 |
| `ignore=[aws]` | 按管理员的优先级，不含 aws | anthropic 与 vertex 之间 |

### 使用上的限制

- 一次请求内能否轮到下一个来源，取决于系统设置的重试次数和重试范围。排在前面的来源渠道多、重试次数少时，可能轮不到后面的来源。
- 不在同一个重试范围内的来源（例如没有写进互通规则的 OpenAI 与 Azure）不会在一次请求中途切换。只有选渠时前面的来源已经没有可用渠道，才会用到后面的来源。
- `only` 会挡掉所选来源不提供的模型。路由是整个令牌一份，不分模型。
- 来源名写在令牌里。管理员改 provider 名、下线来源，或用户换了等级后新等级没有这个来源，引用它的设置就不再生效；打开了 `only` 的令牌会没有可用渠道。
- 命中亲和规则或 `X-Response-ID` 的渠道只要在允许范围内就继续使用，不会因为它的来源排在后面而换回前面的来源。
- 设置了 `order` 后，来源之间的先后由用户决定，管理员的优先级只在来源内部生效。

### 适用范围

适用于所有需要选渠的新请求，包括文本、图片、视频提交、Flux 和 count_tokens。

不适用：

- 管理员在 key 后面指定渠道（`sk-xxx-<渠道 ID>`）。
- 任务查询和基于已有资源的操作（Sora、xAI、Flux、豆包、Runway、Kling、阿里视频的查询等），它们按任务记录找渠道。
- Kling 引用已有资源的 POST 操作会改绑到资源所在的渠道，即使该渠道的来源不在令牌路由允许的范围内。资源只存在于那个渠道。

### 报错

- 路由允许的范围内没有可用渠道：HTTP 503，信息里写明原因是这个 API key 的 provider routing。不会换到范围外的来源。
- Responses 历史要求的 provider 或渠道被路由排除：HTTP 409，并说明原因。不会丢弃历史继续请求。

### 核对请求走了哪个来源

日志列表里每条记录带 `source` 字段，是处理这次请求的渠道的来源名。用户的 `/api/log/self`、`/api/log/self/search` 和管理员的 `/api/log/` 都返回它，控制台的日志页显示为 Source 列。

- 优先用写日志时记录下来的来源；没有记录时按渠道当前的配置取，所以管理员改了渠道的 provider 之后，这部分旧日志显示的是新名字。
- 渠道已经删除、或与渠道无关的日志（充值、管理操作）没有这个字段。
- 失败的请求记的是最后一次尝试的渠道。

### 保存

`POST /api/token` 和 `PUT /api/token` 的请求体里带 `provider_routing`：

- 来源名去掉首尾空格并转小写；每个最长 128 个字符；每个列表最多 32 个且不重复。
- 同一来源不能同时出现在 `order` 和 `ignore` 里。
- `only` 为 true 时 `order` 不能为空。
- 只接受 `order`、`only`、`ignore` 三个字段，其他字段会被拒绝。
- 不校验来源当前是否存在。
- 更新时不带 `provider_routing`、值为 `null`、或与已保存的内容相同，都不改动已有设置。空对象 `{}` 表示清空。只改状态的请求（`status_only`）不动路由。

保存后立即生效：更新和删除令牌时会清除这个令牌的缓存。禁用令牌、修改额度也因此变成立即生效（此前最长延迟一个同步周期）。

直接改库写进了不是合法 JSON 的内容时，这个令牌读取报错、不可用，不会被当成没有设置路由。

## 互通规则

系统设置 `ProviderInteropRules`（模型设置页的 Provider interop rules），JSON 数组：

```json
[{"models":["^claude-"],"providers":["anthropic","vertex","aws"]}]
```

- `models`：模型名正则，匹配请求里的模型名，任一匹配即可。
- `providers`：渠道上填写的 provider 值，至少两个。

行为：

- 首选渠道确定后，如果它的 provider 是 P，取第一条“模型匹配且 `providers` 包含 P”的规则，本次请求的重试范围就是这条规则的 `providers`。没有匹配的规则时，重试范围仍是 P 一个。
- 重试范围来自历史状态时不扩大：Responses 请求带有历史状态，或带了 `X-Linkinfra-Provider` 请求头，仍只用原 provider。
- 没有填 provider 的渠道与之前一样：作为首选时重试不受限制，也不会成为有重试范围的请求的候选。要参与互通的渠道必须填 provider。
- 重试候选是重试范围与令牌路由的交集，先后按上面的选渠规则。
- provider 重试的降档规则不变（原档补重试一次，之后只往更低的优先级，用完即停）。令牌设置了 `order` 时，降档按来源分别计算；否则对整个重试范围一起计算。
- 重试范围被规则扩大时，Responses 状态记录的来源是实际处理请求的渠道的 provider，下一轮续接只用这个 provider。没有扩大时记录的内容与之前相同。
- 重试换到另一个 provider 之后，如果因为不可重试的错误停止（常见的是那个 provider 不支持请求里的某个功能，返回 400），返回给客户端的是首次尝试的错误（通常是限流或过载），而不是后一个 provider 的错误：原 provider 恢复后同样的请求可以成功，客户端应当稍后重试而不是修改请求。后一个 provider 的错误保留在错误日志的重试明细里。其余情况返回的错误不变：可重试的错误用完了渠道，返回最后一次的错误；不可重试的错误来自首选渠道的同一个 provider，照常返回它；已经开始向客户端输出的流式请求不受影响。

保存时校验：必须是 JSON 数组；每条规则只能有 `models` 和 `providers` 两个字段；正则不能为空且可以编译；`providers` 去重后至少两个。不合法时拒绝保存。设置为空或 `[]` 时没有规则。和其他系统设置一样，按同步周期在各实例生效。规则按先后检查，取第一条命中的。

`GET /api/channel/providers`（管理员）返回渠道上出现的 provider、渠道数和启用的渠道数，供编辑规则时核对。

规则只声明“这些 provider 之间可以互相重试”。资源是否真的互通由管理员确认：OpenAI 与 Azure 的 reasoning 密文、AI Studio 与 Vertex 的 Gemini thought signature 不互通，不要写进同一条规则。

## Gemini

AI Studio 和 Vertex 的 Gemini 渠道分别填不同的 provider，不写进互通规则，跨轮次靠亲和规则粘在原渠道。

## 部署

- `tokens` 表新增可空列 `provider_routing`。主节点（`NODE_TYPE=master`）启动时自动加列，也可以先手动执行：

  ```sql
  ALTER TABLE tokens ADD COLUMN provider_routing TEXT;
  ```

- 列还不存在时，读取令牌、创建和更新没有设置路由的令牌都照常工作；只有保存路由会报错。
- PostgreSQL 上加列的瞬间，其他实例上已经预编译的 `tokens` 查询可能各报一次 `cached plan must not change result type`，随后自动恢复，表现为个别请求鉴权或预扣费失败。这是按 GORM 预编译语句的实现和 PostgreSQL 的行为推断的，没有在真实的库上复现过；任何给 `tokens` 加列的发布都一样。介意的话在低峰期手动加列。
- 令牌的 Redis 缓存 key 从 `token:<key>` 换成 `token:v2:<key>`：旧版本实例写的缓存不含路由，新版本不读它。
- 滚动发布期间，请求落到旧版本实例时不受路由限制。后端全部升级后再发布前端入口。
- 设置了路由的令牌，亲和缓存的 key 带令牌 ID（`channel_affinity:v1:token=<id>:...`），各令牌的粘性互不影响。没有设置路由的令牌 key 不变。
