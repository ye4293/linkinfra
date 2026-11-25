# 令牌的来源路由与 provider 互通规则

日期：2026-11-25
后端：`linkinfra`（当前 `main`，开发分支 `token-provider-routing`，在 worktree 中开发）
前端：`~/Desktop/linkinfra-web`（同名分支）
状态：已确认（2026-11-25），实现中

## 1. 背景与目标

现在选渠只看用户等级分组和模型，再按管理员设置的优先级和权重。令牌（`model/token.go`）没有任何路由字段，用户不能决定请求走哪个上游。

渠道的 `config.provider` 目前同时是：失败重试的边界（首选渠道填了 provider，本次请求只在同 provider 内重试）、Responses 状态绑定的来源、模型广场和来源监控的来源名。见 `docs/channel-provider.md`。

目标：

- 用户可以在令牌上设置使用哪些来源、先后顺序，以及永不使用哪些来源。
- Claude 这类跨 provider 可以互通的模型，失败重试可以跨 provider；OpenAI 与 Azure、AI Studio 与 Vertex 的 Gemini 维持不跨。
- 没有设置路由的令牌、没有配置互通规则的模型，行为与现在完全一致。

用词：

- **来源**：用户看到并选择的上游，等于模型广场的 `source_key`。渠道填了 provider 就是 provider，没填就是渠道类型名，统一小写（例如 `anthropic`、`vertex`、`aws`、`azure`）。
- **重试范围**：一次请求失败重试时允许使用的 provider。现在只有首选渠道的那一个 provider。

## 2. 方案设计

### 2.1 令牌路由

`tokens` 表新增一列 `provider_routing`（text，可空），内容为 JSON：

```json
{"order":["vertex","anthropic"],"only":true,"ignore":["aws"]}
```

| 字段 | 对应界面 | 含义 |
|---|---|---|
| `order` | Provider Selection | 选中的来源，顺序即使用的先后 |
| `only` | Only Selected Providers | 为 true 时只用 `order` 里的来源；为 false 时其余来源排在最后作后备 |
| `ignore` | Ignore Providers | 永不使用的来源 |

空值表示没有设置路由。

选渠规则：

1. 候选渠道 = 该分组和模型下启用的渠道，去掉本次请求已失败的、不在重试范围内的、令牌路由不允许的。
2. 令牌路由不允许的渠道：来源在 `ignore` 里；或 `only` 为 true 而来源不在 `order` 里。
3. 先后：按 `order` 逐个来源尝试；`only` 为 false 时，其余来源合并为最后一层。当前来源没有可选渠道才进入下一个来源。首选和失败重试都按这个顺序。
4. 来源内部沿用现有规则：管理员的优先级和权重；provider 重试的降档规则（原档补一次，之后只往更低优先级）按来源分别计算。
5. `order` 为空时只有一层：除 `ignore` 外的所有来源，按管理员的优先级，与现在一致。

示例（Claude 有 `anthropic`、`vertex`、`aws` 三个来源，都在互通规则里）：

| 令牌的设置 | 首选 | 失败后的重试 |
|---|---|---|
| 不设置 | 按管理员的优先级和权重 | 三个来源之间都可以 |
| `order=[vertex]`，`only=false` | vertex | 先 vertex，再到 anthropic、aws |
| `order=[vertex,anthropic]`，`only=true` | vertex | 先 vertex，再到 anthropic，不会到 aws |
| `order=[vertex]`，`only=true` | vertex | 只在 vertex 的渠道内，用完就报错 |
| `ignore=[aws]` | 按管理员的优先级，不含 aws | anthropic 与 vertex 之间 |

`ignore` 的优先级最高。被忽略的来源在以下各处都不会被选中：首选、失败重试、互通规则扩大后的重试范围、`only` 关闭时的后备层、亲和规则和 `X-Response-ID` 命中的渠道、count_tokens。

适用范围：

- 所有需要选渠的新请求，覆盖全部协议（文本、图片、视频提交、Flux 等）。过滤写在选渠函数里（`model/cache.go` 的 `CacheGetRandomSatisfiedChannel`），各协议的重试循环都经过它，不需要逐个修改。
- count_tokens（`CacheGetRandomSatisfiedChannelWithCapability`）使用同一套令牌路由，包括顺序，让计数尽量落在正式请求会用的来源上。它不受重试范围限制，这一点与现在相同。
- 亲和规则或 `X-Response-ID` 命中的渠道，令牌路由不允许时跳过，按正常规则选渠。命中的渠道只要在允许范围内就继续使用，不因为它的来源排在后面而换回前面的来源。
- 不适用：管理员在 key 后面指定渠道（`sk-xxx-<渠道 ID>`）；不经过分发中间件的任务查询和基于已有资源的操作（Sora、xAI、Flux、豆包、Runway 的查询等），它们按任务记录找渠道。
- Kling 和阿里视频的 GET 查询会先经过分发中间件再改绑到任务所在渠道，这两类 GET 在分发时不应用令牌路由。
- Kling 引用已有资源的 POST 操作（`controller/kling_video.go:114`）：分发阶段照常应用令牌路由，随后处理函数改绑到资源所在的渠道，即使该渠道的来源不在令牌路由允许的范围内。资源只存在于那个渠道，本次不改这一行为。

报错：

- 令牌路由允许的范围内没有可用渠道：返回 503，信息写明原因是这个 API key 的 provider routing，不换到范围外的来源。
- Responses 历史要求的 provider 或渠道被令牌路由排除：返回 409 并说明原因，不丢弃历史继续请求。

保存校验（`POST`/`PUT /api/token`）：

- 来源名去掉首尾空格并转小写，每个最长 128，每个列表最多 32 个且不重复。
- 同一来源同时出现在 `order` 和 `ignore` 里：拒绝。
- `only` 为 true 而 `order` 为空：拒绝。
- 不校验来源当前是否存在。来源会随渠道启停变化，由界面标出已不可用的来源。
- `PUT` 请求里不带 `provider_routing` 或值为 `null` 时不改动已有设置；传空对象表示清空。只改状态的请求（`status_only`）不动路由。没有提交路由的更新不写这一列，并发的两个更新不会用各自读到的旧值覆盖对方刚改的路由。
- 库里的内容不是合法 JSON（只可能来自直接改库）时，这个令牌读取报错、不可用，不会被当成没有设置路由。

### 2.2 互通规则

新增系统设置 `ProviderInteropRules`（JSON，与亲和规则一样存在 options 表）：

```json
[{"models":["^claude-"],"providers":["anthropic","vertex","aws"]}]
```

- `models`：模型名正则，匹配请求的模型名（与亲和规则的 `ModelRegex` 相同）。
- `providers`：渠道上填写的 provider 值，至少两个。
- 首选渠道确定后，如果它的 provider 是 P，取第一条"模型匹配且 `providers` 包含 P"的规则，本次请求的重试范围就是这条规则的 `providers`。没有匹配的规则时仍是 P 一个。
- 重试范围来自历史状态时不扩大：Responses 请求带有已绑定的状态，或带了 `X-Linkinfra-Provider`，仍只用原 provider。
- 没有填 provider 的渠道与现在一样：作为首选时没有重试范围（重试不受限制），也不会成为有重试范围的请求的候选。要参与互通的渠道必须填 provider。
- 重试候选是重试范围与令牌路由的交集，先后按 2.1 的规则。
- 保存时校验：必须是 JSON 数组，每条规则只能有 `models` 和 `providers` 两个字段，正则可以编译且不能为空，`providers` 规范化后至少两个。不合法时拒绝保存。
- 设置为空时与现在的行为完全一致。

实现上，请求上下文里的重试边界从一个 provider 字符串改为一组 provider（`model/channel_provider.go`）。

### 2.3 状态、粘性和 provider 读取

- **Responses 状态来源**：互通规则扩大了重试范围时，`service/responses_state.go` 记录来源改为记录实际处理请求的渠道的 provider（请求可能是范围内的另一个 provider 处理的）。没有扩大范围时仍记录首次选渠固定下来的 provider，与现在相同。实现时发现两者并不总是相同（首选渠道没有填 provider、重试到了填了 provider 的渠道时不同），所以只在范围扩大时才改，保证没有规则时行为不变。
- **亲和缓存**：令牌设置了路由时，亲和缓存的 key 加上令牌 ID（`service/channel_affinity.go`）。否则同一用户的两个令牌选了不同来源时，一个令牌命中的渠道不在另一个的允许范围内，会被当作失效删除，两边互相清掉对方的粘性。没有设置路由的令牌 key 不变。
- **provider 的读取**：`ChannelProvider` 改为只解析 `config` 里的 `provider` 一项，不依赖整份 `config` 解析成功。现在 `config` 的其他字段类型不对时 provider 被当成没填，渠道的来源会变成渠道类型名，`ignore` 可能因此挡不住它。保存渠道时拒绝不是字符串的 `provider`，以及超过 128 个字符的 `provider`（令牌路由里的来源名最长 128，更长的既选不中也忽略不了）。
- **Gemini**：不改代码，靠配置。AI Studio 和 Vertex 的渠道分别填不同的 provider（不写进互通规则），跨轮次靠亲和规则粘在原渠道。

### 2.4 接口与缓存

- `GET /api/token/sources`（登录用户）：当前用户分组下可用的来源，每项含来源名、显示名和可用的模型列表。供令牌表单列出选项，并提示打开 `only` 后哪些模型不可用。
- `GET /api/channel/providers`（管理员）：渠道上出现的 provider、渠道数和其中启用的渠道数。供互通规则编辑器标出没有任何渠道使用的 provider。
- 令牌的 Redis 缓存改用新 key `token:v2:<key>`，更新和删除令牌时清除缓存。现在缓存存的是整个 Token，更新令牌不清缓存，修改最长要等一个 `SyncFrequency` 周期（默认 10 分钟）才生效；换 key 是因为旧版本实例写入的缓存不含路由字段，新版本读到会当成没有设置。

### 2.5 前端（linkinfra-web）

1. `sections/token/token-form.tsx`：新增 Provider Routing 区域。已选来源列表可拖动排序（仓库已有 `@dnd-kit`），Only Selected Providers 开关（默认关闭），Ignore Providers 多选。标出已不可用的来源；打开 `only` 时列出会因此不可用的模型。
2. `app/api/token/sources/route.ts`：新增代理。`lib/types/token.ts` 增加字段。
3. `sections/setting/view/modelSettingPage.tsx`：在 Channel affinity 下方增加互通规则编辑器，列出每个 provider 当前的渠道数。
4. `locales/interface-zh.json`：补充中文文案。控制台界面的文案以英文为 key，中文在这个文件里对照（原计划写的是 `locales/en.ts`、`locales/zh.ts`，那两个文件只用于落地页）。管理端的互通规则编辑器与模型设置页一致，只有英文。

### 2.6 可单独确认的两步

- **用户日志显示来源**（已实现）：`/api/log/self` 此前不返回来源，用户无法确认路由是否生效。日志列表的每条记录增加 `source` 字段：优先用日志里已记录的来源，没有时按渠道当前的配置取。不加数据库列。管理员的日志列表和用户的日志搜索接口也一并返回。
- **跨 provider 重试后的错误**：重试循环返回最后一次尝试的错误。请求在一个 provider 上遇到 429、重试到另一个 provider 后因功能不支持得到 400 时，客户端只看到 400。改为：跨到另一个 provider 后得到的不可重试错误不覆盖最初的错误。涉及 `controller/relay.go` 里通用、Claude、Gemini、Responses 四个重试循环。

## 3. 影响范围

- **数据库**：`tokens` 表新增一个可空列，不改已有列，不迁移数据。主节点（`NODE_TYPE=master`）启动时 AutoMigrate 会自动加列；也可以先手动执行：

  ```sql
  ALTER TABLE tokens ADD COLUMN provider_routing TEXT;
  ```

  旧版本实例读写带有新列的表不受影响。新版本实例在列不存在时，读取令牌、创建和更新没有设置路由的令牌都照常工作（不写这一列）；只有保存路由会失败并报错。前端入口在后端全部升级后才发布，所以发布期间实例的启动先后不影响现有功能。
- **加列的瞬间**：GORM 开了预编译语句（`PrepareStmt`）。PostgreSQL 上，表结构变化后，其他实例上已经预编译的 `SELECT * FROM tokens ...` 会各报一次 `cached plan must not change result type`，GORM 随后丢弃这条语句并重新预编译。表现为加列的瞬间有个别请求鉴权或预扣费失败。这是给热表加列的固有现象，与本次改动的逻辑无关；介意的话在低峰期手动执行上面的 `ALTER TABLE`。
- **默认行为**：没有设置路由的令牌、没有互通规则时，选渠和重试与现在一致。选渠函数会被重构，现有的 provider 重试和状态绑定测试用来守住这一点。
- **管理员的跨来源优先级**：令牌设置了 `order` 后，来源之间的先后由用户决定，管理员的优先级只在来源内部生效。
- **一次请求内能否轮到下一个来源**取决于系统的重试次数和重试范围：排在前面的来源渠道多、重试次数少时，可能轮不到后面的来源；不在同一个重试范围内的来源（例如 OpenAI 与 Azure）只有在选渠时前面的来源已经没有可用渠道才会用到。
- **`only` 会挡掉所选来源不提供的模型**：列表是整个令牌一份。
- **来源名写进了令牌**：管理员改 provider 名、下线来源，或用户升档后新等级没有这个来源，都会让引用它的令牌设置失效；开了 `only` 的令牌会没有可用渠道。改 provider 名还会让已有的 Responses 会话续接失败（状态索引按 provider 名记录，保留 7 天）。
- **令牌缓存**：更新和删除令牌立即生效（此前最长延迟一个同步周期），包括状态和额度的修改。
- **发布顺序**：滚动发布期间旧版本实例不识别路由字段，请求落到旧实例时不受路由限制。后端全部升级后再发布前端入口。
- **互通规则的生效时间**：和其他系统设置一样，按同步周期在各实例生效。
- **provider 读取方式的修正**：`config` 其他字段写错的渠道，provider 现在会被正确读出，重试边界对它生效。
- 不影响：计费、渠道折扣、计费标签、来源监控的归属（每次尝试仍记在实际处理它的渠道的来源上）。

## 4. 验证方式

1. 单元测试：路由配置的规范化和校验（大小写、重复、冲突、超限、`only` 无 `order`）；互通规则的解析和校验；来源名的取值（填了 provider、没填、`config` 其他字段写错）。
2. 选渠测试：按 `order` 分层；`only` 开和关；`ignore` 在首选、重试、后备层、亲和命中、`X-Response-ID` 命中、count_tokens 各处都生效；与重试范围取交集；没有设置路由时结果与现在一致。
3. 重试测试：互通规则生效时按令牌顺序跨 provider；没有规则时不跨；历史状态决定的范围不扩大；被忽略的来源不会因互通规则被选中；现有降档规则在单一来源下不变。
4. 状态与粘性：重试后记录的状态来源是实际处理的渠道；设置了路由的两个令牌的亲和缓存互不影响。
5. 接口测试：创建和更新令牌的路由字段；请求不带该字段或为 `null` 时不改动；`/api/token/sources` 只返回当前分组可用的来源；两个新接口的权限；更新令牌后缓存立即失效。
6. 不受路由影响的路径：管理员指定渠道；Kling 和阿里视频的 GET 查询。
7. 变异检查：对 `ignore` 过滤、`only` 过滤、交集、历史范围不扩大这几处，用 `go test -overlay` 套上去掉该行为的源文件副本，对应测试应当失败。
8. 在 worktree 中执行 `go build ./... && go vet ./...` 和 `go test ./...`；前端执行类型检查与构建。
9. 本地联调：全新的临时 SQLite 库（`NODE_TYPE=master`）、模拟上游，建 anthropic、vertex、aws 三个来源的渠道，按 2.1 的示例表逐行验证首选和重试的去向，并在浏览器里走查令牌表单和互通规则编辑器。

## 5. 执行顺序

1. 确认本计划。
2. 后端：provider 读取修正、令牌路由的存储与校验、选渠过滤、令牌缓存、`/api/token/sources` 及测试，单独提交。
3. 后端：互通规则、重试范围、状态来源记录、亲和 key、`/api/channel/providers` 及测试，单独提交。
4. 前端：令牌表单、互通规则编辑器，单独提交。
5. 2.6 的两步，确认后各自单独提交。
6. 更新 `docs/CHANGELOG.md`，新增 `docs/token-provider-routing.md` 使用说明，更新 `docs/channel-provider.md`。

## 6. 不在本次范围

- 截图里的 Ordering（按延迟、价格等指标自动排序）。按模型和来源的延迟与成功率数据已有，留到第二版。
- 按请求指定来源（请求头或请求体）。
- 来源的"用户不可选"开关和来源目录。所有来源都可以被选择，provider 的值原样显示给用户。
- 为 Gemini 的 thought signature 建状态索引。
