# Responses thinking 状态绑定

## 背景与目标
用户进一步确认：同 provider 资源互通，但 thinking 在 OpenAI/Azure 间不互通，需要约束后续请求首次选渠。基于已提交的 Provider 重试限制，增加按实际返回状态追踪来源的会话绑定，不清理任何请求内容。

## 方案设计
- 保存 reasoning/compaction 密文的 SHA-256 指纹、返回 item ID、response ID 到原 provider/渠道/key 的映射；按认证用户隔离，不保存会话正文或密文。
- Redis 保存映射（7 天有效期）；未启用 Redis 时单进程使用有界内存缓存。Redis 启用但故障时不降级到不一致的本地状态。
- 后续 Responses 和 compact 请求携带这些状态时，在首次选渠前查来源并固定 provider；服务端 response/item/conversation 引用额外固定渠道和 key。未填写 provider 时也固定原渠道/key。
- 纯明文请求维持原首次路由。未知/过期/混合来源状态明确拒绝，支持客户端 X-Linkinfra-Provider 指定已有历史的原 provider（仍遵守模型与用户组权限；不能覆盖已知来源）。
- 状态输出转发前写入来源；流式逐项记录，避免必须等 response.completed。流已开始不透明重试。保留原始请求，模型映射使用局部 JSON 修改，修正 Azure compact 端点。
- 前端保留 Provider 输入，更新说明即可；不需要用户额外设置会话 ID，不改变工具链，不迁移数据库、不部署。

## 影响范围
model provider 路由约束、service 状态索引、middleware 首次路由、Responses 转发及相关测试。内存缓存重启或索引过期后必须明确选择原 provider 或开始新会话，禁止静默切换。

## 验证方式
模拟普通及 SSE 上游返回，验证下一轮 reasoning/compaction 原 provider 首选、工具/正文原样、不同用户隔离、未知来源/冲突/缓存故障、空 provider 和服务端引用固定渠道/key、compact 路径及模型映射字段保留。运行 build/vet/相关回归与前端类型检查。
