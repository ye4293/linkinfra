# 智谱渠道支持 Claude 协议自动分发

## 背景与目标

### 背景
linkinfra 当前接入智谱 Claude Code 兼容接口只能用"绕法"：选 **claude（Anthropic）渠道类型** + Base URL 填智谱 anthropic 兼容路径。这导致智谱 GLM 走 Claude 协议的消耗被记到 **claude 渠道类型** 下。后续规划做"各渠道类型消耗排行榜"时，智谱的消耗会串到 claude 类型，统计失真。

new-api 的 `zhipu_4v` adaptor 提供了更好的做法：渠道类型仍是智谱，adaptor 根据**客户端请求格式**自动分发——Claude 协议请求走智谱 anthropic 兼容端点，OpenAI 协议请求走智谱原生端点。这样消耗始终归属智谱渠道类型。

### 目标
让 linkinfra 的**智谱渠道（类型=智谱）**能接收 Claude/Anthropic 协议请求（`/v1/messages`），自动路由到智谱 anthropic 兼容端点 `/api/anthropic/v1/messages`，消耗记在智谱渠道的 `ChannelId` 下，使渠道类型统计口径正确。

### 范围
- **本批**：仅智谱渠道。机制通用，后续 Kimi / MiniMax / 阿里照搬同一模式。
- **不本批**：其他渠道适配、`glm-coding-plan` 套餐别名机制、渠道类型消耗排行 UI。
- 参照实现：new-api `relay/channel/zhipu_4v/adaptor.go`。

## 方案设计

### 机制层（通用，零新字段）
linkinfra 的 `meta.Mode` 已由 `Path2RelayMode` 从请求路径推导（`relay/util/relay_meta.go:101`），而 `/v1/messages` 已映射到 `RelayModeClaude`（`relay/constant/relay_mode.go:52`）。因此 **`meta.Mode == constant.RelayModeClaude` 就是现成的"Claude 原生请求"标识**，任何 adaptor 读它即可分发。

- 不引入新 `RelayFormat` 字段（new-api 用 `RelayFormat`，linkinfra 用已有的 `Mode`，等价）。
- 不改 `Adaptor` 接口（`relay/channel/interface.go`）。
- 不改路由、不改 `RelayMeta` 结构、不改 DB schema。

### 智谱 adaptor 改动（仅 `relay/channel/zhipu/adaptor.go`）

**1. `GetRequestURL` 开头加 Claude 分支**
```go
func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
    // Claude 原生请求 → 智谱 anthropic 兼容端点
    if meta.Mode == constant.RelayModeClaude {
        return fmt.Sprintf("%s/api/anthropic/v1/messages", meta.BaseURL), nil
    }
    // 以下保持现有 v3/v4 逻辑不变
    a.SetVersionByModeName(meta.ActualModelName)
    if a.APIVersion == "v4" {
        return fmt.Sprintf("%s/api/paas/v4/chat/completions", meta.BaseURL), nil
    }
    method := "invoke"
    if meta.IsStream {
        method = "sse-invoke"
    }
    return fmt.Sprintf("%s/api/paas/v3/model-api/%s/%s", meta.BaseURL, meta.ActualModelName, method), nil
}
```
URL 与 new-api `zhipu_4v/adaptor.go:58` 一致。

**2. `SetupRequestHeader` 加 Claude 分支**
Claude 请求用 `Authorization: Bearer <渠道key>`（智谱 anthropic 端点认证方式，跳过 `GetToken` 的 JWT 生成）；否则保持现有 JWT 逻辑。
```go
func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
    channel.SetupCommonRequestHeader(c, req, meta)
    if meta.Mode == constant.RelayModeClaude {
        // 倾向取渠道 key（actual_key）；见下文"渠道 key 取值"，实现时对照确认
        req.Header.Set("Authorization", "Bearer "+meta.ActualAPIKey)
        return nil
    }
    token := GetToken(meta.APIKey)
    req.Header.Set("Authorization", token)
    return nil
}
```
与 new-api `zhipu_4v/adaptor.go:82`（`Bearer`）一致。

**3. `ConvertRequest` 不改**
Claude 原生请求是 body 透传（`relay/controller/claude.go:108` 直接把原始 Claude body 透传给上游），不经 `ConvertRequest`。等价于 new-api `ConvertClaudeRequest` 透传 `req`（`zhipu_4v/adaptor.go:30`）。

**4. `DoResponse` 不改**
Claude 原生响应由 controller 层 `doNativeClaudeStreamResponse` / `doNativeClaudeResponse`（`relay/controller/claude.go:140/143`）统一处理，它们解析的是标准 `anthropic.Response` / `anthropic.StreamResponse`（`claude.go:510/566`）。智谱 anthropic 兼容端点返回标准 Anthropic 响应格式，native handler 直接能处理。`adaptor.DoResponse` 只在 AWS（`resp == nil`）时触发，智谱不涉及。等价于 new-api 委托 `claude.Adaptor.DoResponse`（`zhipu_4v/adaptor.go:115`）。

### 渠道 key 取值（待实现时确认）
智谱 key 应取**渠道 key**，倾向 `meta.ActualAPIKey`（由 `middleware/distributor.go:321` 的 `SetupContextForSelectedChannel` 写入 `c` 的 `actual_key`，`relay_meta.go:115` 读取）。

注意：现有 `anthropic` adaptor 用的是 `meta.APIKey`（=`Authorization` 头剥 Bearer，实为客户端 token，`relay_meta.go:112`）——这本身可能是绕法的一个隐患。实现时对照确认智谱 Claude 分支应取 `meta.ActualAPIKey`，确保发给上游的是渠道密钥而非客户端 token。

### 渠道配置
- 类型=智谱，Base URL 填 `https://open.bigmodel.cn`（留空走默认 `common.ChannelBaseURLs[ChannelType]`，`relay_meta.go:155`）。
- 密钥填智谱 API Key（一把 key 通吃：Claude 分支用 Bearer，OpenAI 分支仍用现有 JWT）。
- 无需模型映射：客户端用 Claude 协议（`/v1/messages`）发智谱模型名（如 `glm-5.2`），body 原样透传；linkinfra 按请求路径自动走智谱 anthropic 端点，不配重定向。渠道模型列表含 `glm-*` 即可（不存在 `claude-*` 走智谱的场景）。

## 影响范围

- **改动文件**：仅 `relay/channel/zhipu/adaptor.go`（约 2 处分支：`GetRequestURL` + `SetupRequestHeader`）。
- **不回归**：现有 OpenAI 协议链路（v3/v4 + JWT 认证）保持不变。
- **不改**：adaptor 接口、`RelayMeta`、路由、DB schema、计费逻辑。
- **统计口径**：智谱 Claude 请求消耗记在智谱渠道 `ChannelId` 下。未来渠道类型排行按 `ChannelId` join `channels.type` 聚合即可，**无需改 Log 表结构**（Log 已有 `ChannelId`，`model/log.go:33`）。
- **不本批**：Kimi / MiniMax / 阿里适配（同模式照搬）、`glm-coding-plan` 别名机制（new-api `constant/channel.go:195` 的 `ChannelSpecialBases`）、渠道类型消耗排行 UI（跨前端仓库 `linkinfra-web`）。

## 验证方式

1. **编译**：`go build ./... && go vet ./...`（CLAUDE.md 强制）。
2. **功能**：配一个智谱渠道（类型=智谱，Base URL=`https://open.bigmodel.cn`，密钥=智谱 API Key），Claude Code 指向 linkinfra 的 `/v1/messages` 发 `claude-3-5-sonnet` 请求：
   - 请求落到 `https://open.bigmodel.cn/api/anthropic/v1/messages`（看请求日志 / 上游 URL）；
   - 响应正常返回（流式与非流式各测一次）；
   - 消耗日志的 `ChannelId` = 智谱渠道 ID（统计口径正确，未串到 claude 类型）。
3. **回归**：同一智谱渠道发 OpenAI 协议 `/v1/chat/completions`（如 `glm-4`），仍走 `/api/paas/v4/chat/completions` + JWT，行为不变。
4. **待实测项**：
   - 智谱 anthropic 端点是否接受 `glm-*` 模型名（如 `glm-5.2`，Claude 协议下原样透传）；
   - 智谱返回是否 100% 标准 Anthropic 格式（native handler 理论兼容，实测确认 cache/usage 字段解析正常）；
   - 渠道 key 取 `meta.ActualAPIKey` 在 Claude 链路是否有值（对照 anthropic adaptor 现状确认）。

## 后续扩展（不在本批）
- 其他国产 anthropic 兼容渠道（Kimi / MiniMax / 阿里）：各自 adaptor 加同一 `meta.Mode == RelayModeClaude` 分支，填各家 anthropic 端点路径。
- 套餐别名机制：仿 new-api `ChannelSpecialBases`，支持 Base URL 填 `glm-coding-plan` 等别名自动展开双端点。
- 渠道类型消耗排行 UI：按 `ChannelId` join `channels.type` 聚合，在 dashboard 或前端仓库实现。
