# 渠道计费标签

渠道编辑页新增可选的 Billing tag（计费标签），例如 `vertex-gemini`、`vertex-claude`。标签把若干渠道归成一组，用来给用户按组设置折扣、按组统计消耗。

标签只管计费和统计，不参与选渠、失败重试和 Responses 状态绑定；这些仍由 Provider 决定，见 `docs/channel-provider.md`。打了标签的渠道照常与其他渠道互相重试。

## 配置

渠道：保存在渠道现有 `config` JSON 的 `billing_tag`，无需数据库迁移。

```json
{"billing_tag":"vertex-claude","region":"global"}
```

- 只允许字母、数字、`.`、`_`、`-`，最长 64。匹配时忽略大小写和首尾空格，前端保存时统一转为小写。
- 含其他字符、超长或不是字符串的标签在保存渠道时被拒绝。
- 多个渠道可以用同一个标签，渠道类型不必相同。
- 清除标签：在渠道编辑页清空 Billing Tag 后保存。直接调用 `PUT /api/channel/` 时，`config` 传空字符串不会生效（空值在更新时被跳过，原 `config` 保持不变）；要清除请传去掉 `billing_tag` 后的 `config`，没有其他配置项时传 `"{}"`。

用户：编辑用户 → Additional discount → Billing-Tag Discount。保存在 `users.channel_ratios`，与渠道类型折扣共用一个 JSON：纯数字 key 是渠道类型，`tag:<标签>` 是计费标签。

```json
{"48":0.9,"tag:vertex-gemini":0.7,"tag:vertex-claude":0.85}
```

编辑页列出渠道上现有的标签，留空表示沿用渠道类型折扣。用户配过、但已没有渠道使用的标签会标为 `Not used by any channel`：折扣仍保存在用户上，在有渠道重新使用该标签之前不会生效，可以在这里移除。

## 折扣取值

用户渠道折扣按以下顺序取第一个命中的值：

1. 渠道有标签，且用户配置了该标签的折扣：用标签折扣。
2. 用户配置了该渠道类型的折扣：用类型折扣。
3. 都没有：1.0。

标签折扣覆盖类型折扣，两者不相乘。计费公式不变：等级折扣 × 渠道折扣 × 用户渠道折扣 × 模型折扣。

以上面的用户配置为例（48 是 Vertex AI）：

| 渠道 | 用户渠道折扣 |
|---|---|
| Vertex AI，标签 `vertex-gemini` | 0.7 |
| Vertex AI，标签 `vertex-claude` | 0.85 |
| Vertex AI，没有标签，或标签未配置折扣 | 0.9 |
| Anthropic，标签 `vertex-claude` | 0.85 |
| Anthropic，没有标签 | 1.0 |

失败重试换了渠道后，折扣按最终成功的渠道重新取值。要让某个用户无论走哪条渠道都是同一折扣，给这些渠道打同一个标签。

## 日志

消费日志的计费明细在命中标签折扣时带 `billing_tag`，管理员在日志详情的折扣明细里可以看到。

标签是管理端的内部分组名：用户查询自己的日志（`/api/log/self`）和资料（`/api/user/self`）时不返回标签，渠道类型折扣照常返回。

## 统计

管理员总览页的 Usage by source 按来源列出最近 24 小时的消耗（`/api/dashboard/stats` 的 `source_stats`）：

- 打了标签的渠道按标签归组，同一标签下不同类型的渠道合并。
- 没有标签的渠道按渠道类型归组。
- 渠道已删除的消耗合并为一项。

统计按日志的 `channel_id` 聚合后再归组，覆盖文本、图片、视频、音频全部消费日志。归组依据是渠道当前的配置：给渠道改标签后，窗口内已有的消耗会跟着算到新的组。

## 接口

`GET /api/channel/billing_tags`（管理员）返回所有渠道上配置的标签及渠道数（含已禁用的渠道），按标签名排序：

```json
{"success":true,"message":"","data":[{"tag":"vertex-claude","channel_count":2}]}
```

## 上线注意

- 前后端需要一起上线。渠道 `config` 在保存时整体覆盖，旧版前端保存渠道会丢掉 `billing_tag`。
- 滚动发布期间，旧实例写入的用户折扣缓存（Redis）不含标签折扣，最长持续一个 `SyncFrequency` 周期。全部实例升级后再配置标签折扣。
- 没有配置标签的渠道、没有配置标签折扣的用户，计费与之前完全一致。
- 视频类路径（`relay/controller/video.go`、`directvideo.go`、`kling_video.go`）只乘模型折扣，不走用户渠道折扣，标签折扣在这些路径上同样不生效。
