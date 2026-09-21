# 渠道 Provider 与同 Provider 重试

## 背景与目标
用户明确收敛范围：仅前端渠道处增加 provider，后端重试限定相同 provider。用户已确认同 provider（如 openai、azure）资源互通。不引入会话分组/绑定，不清理 thinking 或 compaction，不更改首次选渠。

## 方案设计
- 在已有 ChannelConfig JSON 中增加 provider，无数据库新增列。
- 前端渠道表单支持 provider 的加载、编辑、新建与批量创建，留空维持旧行为。
- 首次分发后把 provider 固定在请求 context；重试候选按 provider 过滤，保留现有模型/用户组权限、优先级和权重。
- 配置了 provider 的请求候选耗尽时停止，不重置失败列表，不走旧的最后渠道兜底。未配置的请求保持原行为。
- 覆盖所有复用重试选渠的协议，不修改请求/响应内容及线上配置。

## 影响范围
后端 model、middleware/distributor、controller/retry_policy 和各协议重试兜底；前端实际 linkinfra-web 的 channel-form。无数据库迁移、无线上部署；此功能仅约束单次请求重试，不能保证下一轮对话首次选渠相同。

## 验证方式
测试同 provider 候选、不同 provider 和未配置候选排除、优先级降级、组内耗尽、旧空值行为及配置规范化。执行后端 build/vet/相关回归、前端类型检查，并检查所有兜底不绕过 provider。
