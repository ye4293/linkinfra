# Flux / Replicate 生成图片转存 R2

## 1. 背景与目标

Flux（BFL）与 Replicate 返回的图片 URL 都是**有时效的临时链接**（BFL 约 10 分钟，Replicate 约 1 小时）。
当前 `image.store_url` 与 `image.result` 直接存上游 URL，过期后历史记录里的图片全部失效，客户端也拿不到可长期访问的地址。

目标：在任务成功落库前，把上游图片转存到已有的 Cloudflare R2 存储，`store_url` / `result` 一律写 R2 永久 URL。

## 2. 现状

图片最终 URL 共有 **5 个成功落点**：

| 路径 | 位置 |
|---|---|
| Replicate 同步完成 | `relay/channel/flux/adaptor.go` `handleReplicateSuccess` |
| BFL webhook 成功 | `relay/channel/flux/adaptor.go` `handleSuccessCallback` |
| Replicate webhook 成功 | `relay/channel/flux/adaptor.go` `HandleReplicateCallback` |
| 对账兜底 (BFL) | `controller/flux_reconciler.go` `applyFluxBFLSuccess` |
| 对账兜底 (Replicate) | `controller/flux_reconciler.go` `applyFluxReplicateSuccess` |

已有 R2 代码：`common/cloudflare/r2.go` 的 `UploadImageToR2`（仅接受 base64，需扩展）。

## 3. 方案设计

### 3.1 `common/cloudflare/r2.go`

- 抽出内部 `putObject(ctx, data []byte, objectKey, contentType string) (string, error)`，收拢 S3 客户端与 URL 拼接；`UploadImageToR2` 改为复用它，**对外签名不变**。
- 新增 `r2Config` 配置快照：R2 配置项都是可被后台选项运行时改写的全局变量，"校验 → 上传 → 拼 URL"三步必须用同一份快照，否则并发的配置变更能让对象写进旧 bucket、URL 按新 bucket 拼出来，得到永久 404 的死链。
- 新增 `getS3Client`：按配置快照缓存 `*s3.Client`。`config.LoadDefaultConfig` 每次调用都会扫环境变量、读 `~/.aws/*`，非 AWS 环境下还可能探测 IMDS（一次带超时的失败网络请求）；且每次 `NewFromConfig` 都新建 Transport，让到 R2 的连接无法复用。
- 新增 `IsConfigured()` / `HasPublicURL()` / `IsR2URL()`。
- `generateFileUUID` 改用 `crypto/rand`：原实现只拼时间戳，而 Windows 时钟粒度可达毫秒级，对账器 50 并发时会生成相同对象键、后写入者静默覆盖前者的图片。
- 新增 `MirrorImageURLToR2(ctx, srcURL, keyPrefix string) (string, error)`：下载上游图片并上传 R2。

### 3.2 边界与风控（`MirrorImageURLToR2`）

| 项 | 取值 | 说明 |
|---|---|---|
| 公共访问域 | **必须配置 `CfFilePublicUrl`** | 未配置时直接拒绝转存。S3 API Endpoint 不支持匿名 GET，用它拼出的 URL 需 SigV4 签名，替换掉上游可用的临时链接等于制造永久 401 死链——比不转存更糟 |
| URL scheme 校验 | 仅 `http` / `https` | 拒绝 `file://` 等 |
| 单次下载超时 | 25s | 继承调用方 ctx 的 deadline |
| 下载连接/响应头超时 | 10s | 专用 `http.Client`，不复用 `util.HTTPClient`（其超时由 `RELAY_TIMEOUT` 控制，可能被配成无超时） |
| 代理支持 | `http.ProxyFromEnvironment` | 手写 `Transport` 不继承 `DefaultTransport` 的代理，漏掉会让依赖出口代理的部署 100% 转存失败且只在日志可见 |
| 请求头 | 浏览器 UA + `Accept: image/*` | 与 `relay/controller/image.go` 的下载路径一致，避免被图片 CDN 当爬虫拦截 |
| 下载尝试次数 | 2 次（即重试 1 次，间隔 1s） | 仅对网络错误与 5xx 重试；4xx 立即失败 |
| 体积上限 | 32 MB | `io.LimitReader(body, max+1)` 判超限。足够覆盖 BFL 4MP PNG，同时约束对账器 50 并发下的内存上界 |
| 空响应 | 报错 | 0 字节不上传 |
| Content-Type 校验 | 拒绝 `text/html`、`application/json` 等 | 防止把上游过期错误页当图片存下来 |
| 上传超时 | 30s | 与下载预算独立，慢下载不吃掉上传时间 |
| 上传重试 | 交给 aws-sdk-go-v2 内置 retryer | 不额外包一层 |
| 总预算 | `MirrorTotalBudget` = 25×2 + 1 + 30 = 81s | 各阶段**继承**同一个 ctx deadline，安全阀真实生效 |
| context 解耦 | 只在 `MirrorResultURL` 一层 `WithoutCancel` | 内层若各自 `WithoutCancel` 会剥离 deadline，让总预算变成拦不住任何操作的摆设；而完全不解耦则客户端断连会中断转存 |

对象键：`keyPrefix/YYYY-MM-DD/HHMMSS-<随机>​<ext>`，`keyPrefix` 取 `flux-images`。

扩展名判定顺序：
1. `Content-Type` 能唯一确定格式（jpeg/png/gif/webp/bmp/svg）→ 用它；
2. 否则（`application/octet-stream` 或上游未声明）回退 URL 路径后缀，且同步把上传用的
   `Content-Type` 按扩展名反查修正，避免对象类型与扩展名不一致导致浏览器当附件下载；
3. 都拿不到 → `.jpg` / `image/jpeg`。

### 3.3 `relay/channel/flux/mirror.go`（新文件）

```go
func MirrorResultURL(ctx context.Context, taskID, srcURL string) string  // 转存，失败降级
func BuildReadyResultJSON(taskID, imageURL string) string                // 收口 BFL query 格式 JSON
func StoredSampleURL(taskID string) string                               // 读库里已转存的 URL
```

`MirrorResultURL` 短路条件（原样返回 srcURL）：
1. `srcURL == ""`
2. `!cloudflare.IsConfigured()` —— 无开关，配置齐全即自动生效
3. `!cloudflare.HasPublicURL()` —— 记 warn 日志，见 3.2 第一行
4. `cloudflare.IsR2URL(srcURL)` —— 重复回调幂等

失败降级：记 `logger.Errorf`（含 taskID、原 URL、错误），返回原 URL，任务仍算成功。

`BuildReadyResultJSON` 收口三处逐字重复的「构造 `{id,status:"Ready",result:{sample}}` map → marshal」，
避免将来加字段时漏改一处、导致 `GetFlux` 对不同路径返回结构不一致的 JSON。

### 3.4 5 个落点接入

统一在写 `StoreUrl` / 构造 `result` JSON **之前**替换 URL：

- `handleReplicateSuccess`：**先定格 `duration` 再转存**（否则转存耗时会污染 `total_duration` 代表的上游出图耗时），替换后再构造返回客户端的 `bflResp`。
- `handleSuccessCallback`：先改 `notification.Result.Sample`，**再** `json.Marshal(notification)` 写 `Result`。
- `HandleReplicateCallback`：同 `handleReplicateSuccess`。
- `applyFluxBFLSuccess`：先改 `poll.Result.Sample` 再 marshal。
- `applyFluxReplicateSuccess`：替换 `imageURL` 后再 `BuildReadyResultJSON`。

### 3.5 `from_source=true` 查询路径

上游返回的永远是临时 URL，而库里可能已是永久 URL。若直接透传，同一接口在 `from_source`
开关下会返回两种寿命不同的链接。因此 `QueryResult`（BFL）与 `queryReplicateResult`
在 Ready/succeeded 分支改用 `StoredSampleURL(taskID)` 覆写响应里的 `sample`（查不到才回退上游值）。

### 3.6 对账器并发去重

转存最长 81s，而 reconciler 每 30s tick 一次、记录在 CAS 落库前仍是 `submitted`/`processing`，
会被后续几轮重复选中并发下载同一张图。新增 `fluxInflight sync.Map` 按 task_id 去重，
在占用并发槽之前先抢占对账权。

### 3.7 已知取舍：webhook 路径阻塞

webhook handler 会等转存完成才返回 200（典型 1~3s，上限 81s）。若上游因超时重发 webhook，
第二次请求可能再转存一份 → R2 多一个孤儿对象；但 DB 侧由 `UpdateIfNotTerminal` 的 CAS 保证
只有一条终态写入、只扣费一次，数据一致性不受影响。各阶段超时已按"尽量落在上游 webhook
投递超时之内"收紧，接受该取舍。

## 4. 影响范围

- **不改**计费、CAS（`UpdateIfNotTerminal`）、状态机。
- **不改**数据库 schema，无迁移；`detail` 字段仍保留上游原始响应（含原 URL）。
- Replicate 同步返回路径响应时间 +1~3s（已与用户确认接受）。
- 旧记录不回填（临时 URL 多已过期）。
- **上线前提**：必须配置 `CfFilePublicUrl`（公共访问域），否则转存整体不生效，行为与改动前一致。

## 5. 验证方式

1. `go build ./... && go vet ./...`
2. 单元测试 `common/cloudflare/r2_mirror_test.go` + `relay/channel/flux/mirror_test.go`：
   - 未配置 R2 / 未配置公共访问域 / 已是 R2 URL → 返回原 URL，不发起下载
   - 上游 404（不重试）、5xx（重试到上限）、HTML 响应、空响应、超出体积上限 → 报错并降级
   - 非法 scheme → 报错
   - `IsR2URL` host 边界（`cdn.example.com.evil.net` 不得误判）
   - 对象键 1000 次生成无重复
3. 端到端：真实 flux 渠道发一次同步请求，确认响应 `result.sample` 为 R2 公共域且可匿名访问，DB `store_url` 一致。

## 6. 后续可做（本次未做，避免扩大改动范围）

仓库里「LoadDefaultConfig + NewFromConfig(UsePathStyle)」的 S3 客户端样板已有 5 份副本
（`common/cloudflare/r2.go`、`relay/channel/video_helper.go:126`、`controller/fileGo.go` 三处），
region 取值已经不一致（`auto` vs `us-east-1`），`fileGo.go` 还硬编码了 `pub-*.r2.dev` 域名而忽略
`CfFilePublicUrl`。它们都可以收敛到本次抽出的 `putObject`。属既有债务，宜单独一次改动处理。
