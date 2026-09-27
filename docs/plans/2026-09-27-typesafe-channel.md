# TypeSafe 独立渠道配置

## 目标
补齐用户指出的渠道类型与默认 Base URL，让后台可以直接创建 TypeSafe 渠道。

## 方案
- 追加实际渠道类型 TypeSafe（50），原有实际类型 1～49 不变；更新末尾计数哨兵。
- 注册 provider、默认地址 `https://api.typesafe.ai`、模型列表和模型详情；专用原生协议仍走现有 System One 处理器。
- 渠道类型接口提供默认地址，实际前端 `../linkinfra-web` 选择 TypeSafe 时自动填入地址、三个 Jev 模型和测试模型。
- 获取上游模型兼容 TypeSafe 的 `models[].name`，并规范化 `/v1`、`/v1/systemone` 地址。
- 保持现有自定义 type 8 配置兼容，无数据库迁移。

## 验证
测试渠道注册、默认地址回退、模型目录、原生模型发现与既有 System One 计费和重试。后端相关包回归、完整 build/vet；前端类型检查和构建。
