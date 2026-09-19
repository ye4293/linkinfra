# 阿里百炼模型接口更新与实测

## 背景与目标

用户已提供业务空间地址与测试凭据，并授权验证和修改。按上一轮表格第 1、2、3、6 项实施：更新模型目录、补充思考控制和工具调用参数、验证 Messages 扩展；同时修正调研中确认已停止维护的 Responses 路径。新域名兼容随实测覆盖。

## 方案设计

- 更新 `relay/channel/ali/constants.go` 中官方明确列出的文本、代码、视觉、Omni 和文本向量模型，保留旧模型兼容。
- 修正 `relay/channel/ali/adaptor.go` 的 Responses URL；核对 Ali Chat 参数转换，保留 `preserve_thinking`、`tool_stream`、`parallel_tool_calls` 的显式布尔值。
- Chat/Embedding 使用原始 JSON 局部改写模型名，保留扩展参数及显式 false/0，不扩充通用 DTO；保留 `max_tokens` 与 `max_completion_tokens` 各自语义。避免主动覆盖模型的思考默认行为，推理参数范围以精确型号及上游校验为准。
- Messages 继续使用原始 JSON 透传，验证 `output_config.effort`、`output_config.format` 和思考内容/工具调用。
- 添加真实请求转换/路由回归测试及可选实测测试，记录实际支持范围与限制。

## 影响范围

主要影响 Ali 模型目录和参数保留，不修改通用 DTO 和其他渠道。无数据库迁移，不更改部署和生产渠道配置。凭据只通过进程环境传给测试，不写入代码、文档、测试结果或提交。

## 验证方式

1. 使用用户指定的业务空间地址，小输出预算验证模型可用性、Chat 思考/工具参数、Messages JSON Schema/effort 及 Responses 新路径。
2. 单元测试验证显式 false 保留、模型重写、`-internet` 兼容、URL 拼接以及扩展请求字段。
3. 在不包含 `output/` 本地草稿的隔离检出目录运行 `go build ./...`、`go vet ./...` 和相关测试。
4. 更新变更记录与实测结果，单独提交本任务修改，不包含已有用户文件改动。

## 实施结果

- 已更新模型目录，包含 Qwen 3.8/3.7、新 Coder/VL/Omni 和文本向量模型，保留历史模型。
- Ali Chat/Embedding 已使用原始 JSON 保留参数，`preserve_thinking`、`parallel_tool_calls`、`tool_stream`、工具 Schema 与显式 false/0 均不会被通用 DTO 裁剪。模型映射及 `-internet` 后缀仍有效。
- Responses 已改为 `/compatible-mode/v1/responses`；支持业务空间根域名，未设置地址时回退默认 DashScope 域名。
- 鉴权优先使用渠道本次实际选中的 `ActualAPIKey`，缺省时回退 `APIKey`。
- Messages 保持既有原始请求体透传，不转换 `output_config` 或擅自将 `budget_tokens` 换算为 effort。

### 实际上游验证

使用用户提供的北京业务空间地址，通过项目 Ali 适配器发起请求。下列最终用例均通过；测试通过并不代表该账号已验证整个内置目录或所有模态。

| 用例 | 验证结果 |
|---|---|
| `qwen3.8-flash` / `qwen3.8-max` / `qwen3.7-plus` / `qwen3-coder-next` | HTTP 200，返回预期文本及 usage |
| `text-embedding-v4` / `qwen3.7-text-embedding` / `qwen3.7-text-embedding-flash` | HTTP 200，各返回 256 维向量 |
| `preserve_thinking:true` 两轮调用 | 两次 HTTP 200；第一轮包含 reasoning_content，完整回传后第二轮答案正确 |
| Chat `parallel_tool_calls:true`，流式与非流式 | HTTP 200，均返回北京、上海两个独立工具调用，参数 JSON 正确 |
| Chat `tool_stream:true`，复杂 array 参数 | HTTP 200，8 个城市的数组经 9 个非空参数片段输出并成功拼接 |
| Messages `thinking.enabled` + `output_config.effort:low` + JSON Schema | 流式、非流式均 HTTP 200，包含思考内容，最终 JSON 严格符合测试的两字段结构 |
| Responses 新路径 | 流式、非流式均 HTTP 200，正常完成并返回文本 |

首次非流式工具用例误带 `stream_options`，上游返回 HTTP 400，提示该参数必须与流式设置一起使用。已修正测试请求，仅流式请求携带该参数，并重跑工具用例通过；未通过修改网关来隐藏无效参数。

实测入口：`relay/channel/ali/live_test.go`。需设置 `ALI_VERIFY_API_KEY` 和 `ALI_VERIFY_BASE_URL` 才会执行，默认跳过。最终 15 个子用例在首次执行及修正后的定向补测中全部通过。

### 本地验证

- `go test ./relay/channel/ali -count=1`：通过。
- 隔离工作树执行 `go build ./...`：通过。
- 隔离工作树执行 `go vet ./...`：通过。
- 隔离工作树执行 `go test ./relay/channel/ali ./relay/util -count=1`：通过，未设置凭据时在线用例跳过。
- 编译器报告已有 sqlite3 C 依赖的局部地址警告，不影响以上检查通过。

### 本批边界

未新增 Responses 查询/删除/输入列表、Session 缓存请求头、缓存计费映射、重排序或 Realtime；未修改既有价格配置、数据库、服务进程或部署。新增目录中未实测的视觉/Omni 型号仅依据官方文档列入，不宣称已验证音视频输入输出。
