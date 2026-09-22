# 模型广场按配置来源去重

## 背景与目标
用户已明确：同一模型按来源分别展示价格，同一来源的多个渠道合并；来源优先使用渠道 config.provider，未配置则使用渠道类型，不要求最低价代表。

## 方案设计
- common/model-provider.go：为各渠道类型提供独立来源名称，加入配置 provider 优先、忽略大小写和首尾空格的目录来源解析；未知类型按类型编号区分。
- controller/model_plaza.go：按解析后的来源和模型名去重，以最小启用渠道 ID 稳定选择代表价格。
- controller/qianfan_test.go：验证配置优先、类型回退、不同来源独立、同来源合并、稳定价格、计数、分页及详情一致。
- docs/CHANGELOG.md：记录最终规则及验证结果。前端现有卡片、筛选和详情直接使用返回的 provider。

## 影响范围
调整公开目录的来源与价格代表选择，并同步渠道类型的来源名称映射。保留已有渠道详情链接；不改变实际路由、状态兼容规则或计费，不需要数据库迁移。

## 验证方式
运行 controller/common 回归测试；在隔离检出目录运行完整 go build ./... 和 go vet ./...。覆盖 OpenAI/Azure、百度/DeepSeek、Anthropic/AWS、Google/Vertex AI、聚合平台与显式 provider 跨类型合并场景。
