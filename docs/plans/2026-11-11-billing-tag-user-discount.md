# 计费标签与按标签的用户折扣

日期：2026-11-11
后端：`linkinfra`（当前 `main`，开发分支 `billing-tag`）
前端：`~/Desktop/linkinfra-web`（同名分支）
状态：已确认（2026-11-11），按本计划实施；实施中的补充见文末。

## 1. 背景与目标

用户折扣按渠道类型存储（`users.channel_ratios`，`middleware/distributor.go` 用 `channel.Type` 取值）。Vertex AI 这类一个类型下同时提供 Gemini 和 Claude 的渠道，即使拆成两个渠道，同一用户也只能有一个折扣。

目标：

- 同一渠道类型下的不同渠道，可以对同一用户设置不同折扣。
- 可以按同一口径统计消耗。
- 不改变选渠、失败重试和 Responses 状态绑定。

不复用渠道 `provider`：它是重试边界和状态兼容组（见 `docs/channel-provider.md`），首次渠道配置后失败重试不再跨 provider；它同时是模型广场的来源名。

## 2. 方案设计

### 语义

- 渠道新增可选的「计费标签」，存于 `config.billing_tag`。匹配时忽略大小写和首尾空格，只允许 `a-z 0-9 . _ -`，最长 64。多个渠道可以用同一个标签。
- 用户折扣沿用 `users.channel_ratios` 这个 JSON：数字 key 仍是渠道类型，新增 `tag:<标签>` key 表示计费标签折扣。
- 取值顺序：渠道有标签且用户配置了该标签，用标签折扣；否则用渠道类型折扣；都没有则为 1.0。标签折扣是覆盖，不是叠乘。
- 计费公式不变：等级折扣 × 渠道折扣 × 用户渠道折扣 × 模型折扣。
- 标签只用于计费和统计，不参与选渠、重试、状态绑定和模型广场。

示例（48 是 Vertex AI 的渠道类型）：

| 对象 | 配置 |
|---|---|
| 渠道 A | Vertex AI，模型 `gemini-*`，`billing_tag=vertex-gemini` |
| 渠道 B | Vertex AI，模型 `claude-*`，`billing_tag=vertex-claude`，`provider` 留空 |
| 用户 X | `{"48":0.9,"tag:vertex-gemini":0.7,"tag:vertex-claude":0.85}` |

X 走渠道 A 按 0.7，走渠道 B 按 0.85，走其他没有标签的 Vertex 渠道按 0.9。Claude 请求在渠道 B 失败后重试到 Anthropic 官方渠道，按 X 的 Anthropic 类型折扣计算（未配置为 1.0）。

### 后端（linkinfra）

1. `model/channel.go`：`ChannelConfig` 增加 `BillingTag`（`json:"billing_tag,omitempty"`）。
2. `model/channel_billing_tag.go`（新增）：`ChannelBillingTag(channel)` 规范化读取标签；`UserChannelRatios`（类型折扣加标签折扣）及其 `Resolve(channelType, tag)`，取值顺序只写在这一处。
3. `model/cache.go`：`decodeChannelRatiosJSON` 同时解析数字 key 和 `tag:` key；`CacheGetUserChannelRatios` 返回 `UserChannelRatios`，写缓存时保留两类 key。Redis key 和失效逻辑不变。
4. `middleware/distributor.go`：`SetupContextForSelectedChannel` 改用 `Resolve`，并把命中的标签写入 `billing_tag`。每次选渠（含重试）都重写，包括空值。
5. `relay/util/ratio.go`：`GetAsyncBillingGroupRatio` 已按 `channelId` 取渠道，同样改用 `Resolve`。
6. `relay/controller/helper.go`：`enrichBillingDetailsFromContext` 在命中标签折扣时写入 `billing_tag`。
7. `controller/channel.go`、`router/api-router.go`：新增 `GET /api/channel/billing_tags`（管理员），返回所有渠道上出现的标签及渠道数，在 `/:id` 之前注册。

### 前端（linkinfra-web）

1. `sections/channel/channel-form.tsx`：新增 Billing tag 输入，包括校验、加载、默认值和 `buildConfig`，覆盖单个创建与批量创建；可从已有标签中选择。
2. `app/api/channel/billing_tags/route.ts`：新增代理，参照 `app/api/channel/types/route.ts`。
3. `sections/user/channel-ratios-editor.tsx`：增加按计费标签设置折扣的区域。标签从接口列出，不手填；用户已配置但已没有渠道使用的标签标记为未使用，可删除。类型区的 Reset all 和批量设置只作用于类型 key。
4. `locales/en.ts`、`locales/zh.ts`：补充说明文案（标签折扣优先于类型折扣）。
5. `sections/log/tables/`（`index.tsx`、`log-table.tsx`、`columns.tsx`）：折扣明细里显示命中的计费标签。

界面文案用英文。

### 统计（第二步，可单独确认）

- `model/dashboard_stats.go`：管理员 24 小时统计增加来源消耗排行。按 `channel_id` 聚合日志，再在内存中映射：有标签的渠道按标签归组，没有标签的按渠道类型归组，渠道已删除的归为 `deleted`。返回项带 `kind` 区分标签和渠道类型。
- 前端 `sections/overview/view/overview.tsx`、`lib/types/dashboard.ts`：总览页增加对应卡片。
- 不给 `logs` 加列，覆盖文本、图片、视频、音频全部消费日志。统计反映渠道当前的标签，不是历史快照。

## 3. 影响范围

- 不改表：渠道标签在 `config` JSON 里，用户折扣沿用 `users.channel_ratios`。旧数据不受影响，旧版本后端会忽略 `tag:` key。
- 没有配置标签的渠道和没有配置标签折扣的用户，计费与现在完全一致。
- 不改选渠、重试、`provider`、状态绑定和模型广场。
- 重试换渠道后，折扣按最终成功的渠道计算（现有行为）。要让某个用户走哪条渠道都是同一折扣，给这些渠道打同一个标签即可。
- 渠道 `config` 在保存时整体覆盖（`controller/channel.go` 的 `updateChannelFields`），旧版前端保存渠道会丢掉 `billing_tag`，前后端需要一起上线。
- 滚动发布期间，旧实例写入的 Redis 缓存不含标签折扣，最长持续一个 `SyncFrequency` 周期。全部实例升级后再配置标签折扣。
- 视频类路径（`relay/controller/video.go`、`directvideo.go`、`kling_video.go`）目前只乘模型折扣，不走用户折扣，本次不改。
- 与待确认的《条件与阶梯计费接入计划》兼容：折扣因子的数量和名称不变。

## 4. 验证方式

1. 单元测试：`channel_ratios` 解析（数字 key、`tag:` key、非法值、大小写）；取值顺序（标签命中、标签未配置时回落类型、渠道无标签、都未配置为 1.0）；缓存读写后两类 key 都保留。
2. 选渠测试：同一请求先后选中两个渠道，`user_channel_ratio` 和 `billing_tag` 随渠道切换；异步路径 `GetAsyncBillingGroupRatio` 结果一致。
3. 接口测试：`/api/channel/billing_tags` 的去重、计数和权限。
4. 回归：现有折扣测试（`relay/util/model-discount_test.go` 等）和 provider 重试测试保持通过。
5. 在 worktree 中执行 `go build ./... && go vet ./...` 和相关 `go test`；前端执行类型检查与构建。
6. 手工验证：两个 Vertex 渠道分别打标签，给测试用户配置两个标签折扣，查看消费日志的折扣明细；让首个渠道失败，确认能重试到其他渠道且折扣随之变化。

## 5. 执行顺序

1. 确认本计划（标签命名、覆盖语义、统计放在总览页）。
2. 后端：标签解析、折扣取值、日志明细、标签列表接口及测试，单独提交。
3. 前端：渠道表单、用户折扣编辑器、日志明细，单独提交。
4. 统计：后端聚合和前端总览卡片，单独提交。
5. 更新 `docs/CHANGELOG.md`，补充 `docs/billing-tag.md` 使用说明。

## 6. 实施补充

实施时在计划之外增加的内容：

- 保存渠道时校验标签：含非法字符或超长的标签直接拒绝，不会保存后静默失效。
- 标签不返回给终端用户：`/api/log/self` 的计费明细和 `/api/user/self` 的 `channel_ratios` 去掉标签项，管理员视图不变。
- 来源消耗返回所有有消耗的来源（标签和渠道类型的数量有限），不截取前 N 名。
