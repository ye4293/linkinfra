# 阿里百炼模型接口文档同步与接入差异

核对日期：2026-09-18。范围：Qwen（渠道类型 17）的 Chat、Messages、Responses、Embedding，以及相关工具与多模态能力。

后续实施：用户已授权并完成本批修改与实测，详见 [实施计划与验证结果](plans/2026-09-18-ali-model-api-update.md)。下文保留修改前的调研快照，其中 Responses 旧路径、Chat 参数裁剪和模型目录差异已在本批处理。

本记录根据当日实际获取的阿里云官方文档和当前工作区代码整理。“新增/差异”指相对于项目已有接入和 2026-04-20 接入记录的差异，不代表这些功能都在今天发布。未调用付费模型接口，未验证账号的模型开通状态；本次仅更新调研文档。

## 1. 优先修复：Responses 旧路径已停止维护

官方《创建响应》明确说明：旧路径 `/api/v2/apps/protocols/compatible-mode/v1/responses` 已停止维护，不再保证功能可用性，要求迁移到 `/compatible-mode/v1/responses`。

当前 `relay/channel/ali/adaptor.go` 的 `RelayModeOpenaiResponse` 分支仍使用旧路径。这是兼容性修复的首要项，不能仅更新模型名。

| 项目对外路径 | 官方当前上游路径 | 当前差异 |
|---|---|---|
| `POST /v1/chat/completions` | `/compatible-mode/v1/chat/completions` | 路径一致，部分参数缺失 |
| `POST /v1/messages` | `/apps/anthropic/v1/messages` | 路径一致，新扩展需验证 |
| `POST /v1/responses` | `/compatible-mode/v1/responses` | 项目仍使用停止维护的旧路径 |
| `POST /v1/embeddings` | `/compatible-mode/v1/embeddings` | 已有文本向量路径，模型目录较旧 |
| 尚无对应路由 | `GET /compatible-mode/v1/responses/{response_id}` | 获取已存储响应 |
| 尚无对应路由 | `DELETE /compatible-mode/v1/responses/{response_id}` | 删除已存储响应 |
| 尚无对应路由 | `GET /compatible-mode/v1/responses/{response_id}/input_items` | 查询输入及历史，支持分页 |

项目虽然注册了 `/v1/responses/compact`，但 Ali 适配器没有独立 compact 路径；本次读取的阿里 Responses 文档也未列出 compact，不能据项目公共路由认定阿里支持。

## 2. 业务空间专属域名

官方推荐使用 `https://{WorkspaceId}.{region}.maas.aliyuncs.com`，例如北京的 `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com`。文档同时列出新加坡、弗吉尼亚、法兰克福、东京等区域配置，Chat/Responses 还列出香港。

旧 `dashscope.aliyuncs.com`、`dashscope-intl.aliyuncs.com` 域名仍可使用；这与 Responses 旧路径停止维护是两件事。

项目支持渠道自定义 BaseURL，根域名可以配置为新域名。配置值应是根地址，不能直接填官方 SDK 示例中的 `/compatible-mode/v1`，否则当前适配器会重复拼接路径。仅修改域名不会修复 Responses 旧路径。

## 3. 模型目录明显落后

官方当前目录及协议参考列出以下模型，项目 `relay/channel/ali/constants.go` 尚未内置：

| 类别 | 官方当前模型示例 |
|---|---|
| 通用 / 推理 | `qwen3.8-max`、`qwen3.8-max-0902`、`qwen3.7-plus`、`qwen3.8-flash` |
| 代码 | `qwen3-coder-next`、`qwen3-coder-flash` |
| 视觉 | `qwen3-vl-plus`、`qwen3-vl-flash`；新一代通用模型也具备视觉能力 |
| 全模态理解 | `qwen3.8-omni-flash` |
| 文本向量 | `text-embedding-v4`、`qwen3.7-text-embedding`、`qwen3.7-text-embedding-flash` |
| 多模态向量 / 重排序 | `qwen3-vl-embedding`、`qwen3.7-text-rerank`、`qwen3-vl-rerank` |

百炼还提供 DeepSeek、Kimi、GLM、MiniMax 等第三方模型。模型 ID、直供供应商、地域和协议支持范围应分别核对，不能把 Chat 支持列表直接当成 Messages/Responses 的完整能力列表。

Ali 适配器没有用内置 ModelList 对请求模型名做白名单校验，因此“未内置”不等于“必须改代码才能调用”。符合已有协议的新模型可配置渠道模型后验证，同时补充定价、上下文和能力信息。

## 4. Chat 参数缺口

Ali Chat 经过 `GeneralOpenAIRequest` 反序列化和重新序列化。该结构没有定义的顶层字段会丢失；当前 `compatibleChatRequest` 只额外提供由 `-internet` 后缀触发的 `enable_search`。

| 官方能力 / 参数 | 当前实现判断 |
|---|---|
| `preserve_thinking`：把历史 assistant 的 `reasoning_content` 纳入输入 | 顶层开关缺失；消息内 `reasoning_content` 已有字段 |
| `reasoning_effort`、`thinking_budget` | 两字段已有；Qwen 3.8 的互斥及取值规则未做专门校验 |
| `tool_stream`：复杂工具参数流式输出 | 顶层字段缺失 |
| `parallel_tool_calls`：并行工具调用 | Chat 请求结构缺失；Responses 请求结构已有同名字段 |
| `enable_search`、`search_options` | 用户直接传入的字段不能完整保留；现有后缀机制只注入开关 |
| 搜索 `turbo` / `max` / `agent` / `agent_max`、强制搜索 | 属于 `search_options`，当前无法完整表达，支持模型各不相同 |
| `enable_code_interpreter` | 顶层字段缺失 |
| `vl_high_resolution_images` | 顶层字段缺失 |
| `use_multichannel`：Omni 空间音频 | 顶层字段缺失 |
| `logprobs`、`top_logprobs` | 顶层字段缺失 |
| `response_format` / `json_schema` | 已有结构，不能算未接入；需按具体模型验证约束效果 |

Qwen 3.8 的 Chat `reasoning_effort` 支持 low、medium、xhigh 等档位与兼容映射，不能与 `thinking_budget` 同时设置。官方还强调历史思考内容应通过 `reasoning_content` 回传，不能拼进 `content`。文档对部分型号默认值及缺失历史思考的说明存在细节差异，落地时应按精确模型 ID 验证。

`Message.Content` 是 `any`，所以内容块中的 `video_url`、像素参数、`cache_control` 等并不因缺少专门 Go 字段就必然丢失；应区分顶层字段与内容块字段。

## 5. Responses：工具、会话及缓存

官方当前列出的工具包括：`web_search`（联网搜索）、`web_extractor`（网页抓取）、`code_interpreter`、`web_search_image`（文搜图）、`image_search`（图搜图）、`file_search`（知识库检索）、`mcp` 和自定义 `function`。

这些工具并非所有模型均支持。例如 `qwen3.8-omni-flash` 的内置工具仅支持 `web_search`，同时支持自定义 function。网页抓取需配合联网搜索；知识库工具当前只接受一个知识库 ID。

`previous_response_id` 简化多轮上下文管理；`store` 默认 true，设为 false 后不能通过响应 ID 继续会话或调用后续查询接口。`background` 当前不支持，不能把创建响应返回对象中的状态枚举理解成已经支持后台异步生成。

Session 缓存通过请求头 `x-dashscope-session-cache: enable` 开启。Ali 的请求头处理未透传该客户端请求头，但公共渠道自定义请求头机制可以注入固定值，因此是“缺少按请求透传”，不是完全无法配置。

现有原生 Responses 控制器在没有模型映射时保留原始请求体；发生模型映射时会重建 `OpeanaiResaponseRequest`，有两类风险：

- 结构未定义的扩展字段会丢失。
- `store`、`parallel_tool_calls` 等 `bool` 字段带 `omitempty`，显式 false 会消失。尤其阿里 `store` 默认 true，可能改变调用方设置的语义。

新增响应 GET/DELETE/input_items 接口时，还需要设计响应 ID 对应的用户与渠道归属。查询请求没有 model，不能简单套用创建请求的模型分发规则。

## 6. Messages：推理强度与结构化输出

官方新增/完善了 `output_config.effort` 与 `output_config.format`（JSON Schema）说明，并将 `thinking.budget_tokens` 标为即将废弃，建议新接入采用 effort。

当前 `RelayClaudeNative` 使用原始请求体，模型映射也通过局部 JSON 改写处理，因此新字段有透传基础；应优先补调用示例和协议验证，不必仅为新增字段重写整个适配器。

显式缓存可放在 system、文本、工具调用和工具结果等内容块的 `cache_control` 中。不同模型对结构化输出可能提供严格 Schema 约束或降级为普通 JSON，不能统一承诺严格模式。

官方本兼容协议只提供 Messages，不提供其 `/v1/models` 接口。本次文档未证明存在阿里原生 `/messages/count_tokens` 支持。

## 7. 缓存计量与多模态

官方同时提供隐式缓存、显式缓存及 Responses Session 缓存。Chat 显式缓存示例返回 `usage.prompt_tokens_details.cache_creation_input_tokens` 和 `cached_tokens`。项目 `relay/model/misc.go` 定义的是 `cache_write_tokens`，没有同名 `cache_creation_input_tokens`，应核对缓存写入计量映射，不能把“请求可透传”直接当成“缓存计费已完整支持”。

`qwen3.8-omni-flash` 支持文本、图片、音频、视频输入，输出为文本，模型页标明 1M Token 上下文，并支持空间音频理解。它不等同于语音输出模型；实时音视频对话和离线语音输出需另选对应 Omni / Audio 模型。Responses 自身还预留约 20% 上下文空间，不能把模型窗口大小直接作为该接口最大输入。

文本向量已有基础通路；多模态向量、重排序、Realtime 不能根据基础 Chat/Embedding 路由就认定完整支持。本次未对这些独立接口以及图像、视频生成做全链路审计。官方目录另列 `qwen-image-3.0-pro`、`wan3.0-video` 等新模型，可另行核对现有图像/视频模块。

## 8. 建议实施顺序

1. 修正 Responses 上游路径；验证流式/非流式，以及模型映射下 `store:false` 等语义保留。
2. 补齐 Ali Chat 扩展参数保留，支持 Session 缓存请求头，核对缓存 usage 映射。
3. 更新模型目录及精确型号的定价/上下文/能力元数据，补充新域名配置示例。
4. 增加 Responses 查询、删除、输入列表及归属管理；随后按需求接重排序、多模态向量、Realtime。

验证时应覆盖：新旧域名、新 Responses 路径、Chat 扩展字段实际送达、reasoning 参数互斥、模型映射保留显式 false、缓存读写 usage、工具调用流式事件，以及新增查询接口的归属校验。

## 官方来源

下列日期是本次 HTTP 获取页面的 `last-modified` 元数据，不是功能发布日期。

| 文档 | 页面更新时间 |
|---|---|
| [OpenAI 兼容 Chat](https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-chat-completions) | 2026-09-18 |
| [创建 Responses 响应](https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-responses) | 2026-09-18 |
| [获取响应](https://help.aliyun.com/zh/model-studio/retrieve-a-response) | 2026-09-11 |
| [删除响应](https://help.aliyun.com/zh/model-studio/delete-a-response) | 2026-09-11 |
| [获取输入项列表](https://help.aliyun.com/zh/model-studio/list-input-items) | 2026-09-11 |
| [Anthropic 兼容 Messages](https://help.aliyun.com/zh/model-studio/anthropic-api-messages) | 2026-09-16 |
| [模型选择](https://help.aliyun.com/zh/model-studio/models) | 2026-09-18 |
| [深度思考](https://help.aliyun.com/zh/model-studio/deep-thinking) | 2026-09-18 |
| [上下文缓存](https://help.aliyun.com/zh/model-studio/context-cache) | 2026-09-18 |
| [联网搜索](https://help.aliyun.com/zh/model-studio/web-search) | 2026-09-18 |
| [结构化输出](https://help.aliyun.com/zh/model-studio/qwen-structured-output) | 2026-09-18 |
| [代码解释器](https://help.aliyun.com/zh/model-studio/qwen-code-interpreter) | 2026-09-16 |
| [向量与重排序](https://help.aliyun.com/zh/model-studio/embedding-rerank-model) | 2026-09-02 |
| [Qwen3.8-Omni-Flash](https://help.aliyun.com/zh/model-studio/qwen3-8-omni-flash) | 2026-09-18 |

本地抓取快照保存在 `output/ali-docs-2026-09-18/`（调研产物，不作为源代码提交）。
