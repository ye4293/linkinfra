# Playground 使用后台可用模型

原因：Playground 使用 /api/models 的内置适配器模型目录，未按实际配置的渠道筛选。

修改：新增需登录的 GET /api/user/models，从 abilities 与 channels 联表查询当前用户分组的启用模型，去重、排序并排除空值；前端新增 /api/playground/models 代理并关闭缓存，Playground 改用此接口。已有模型选择失效或返回空列表时清空选择；提示统一使用英文。渠道管理继续使用原有目录接口。

验证：后端覆盖自定义模型、跨渠道去重、用户分组隔离、渠道及能力禁用、空列表、查询失败和身份认证；前端运行类型和 lint 检查。

结果：`go test ./controller ./model`、`go build ./...`、`go vet ./...`、前端 `tsc --noEmit --incremental false` 及改动文件的 Next.js lint 均通过。Playwright 使用模拟模型与令牌数据验证 1440px 桌面及 390px/320px 手机布局、下拉边界、搜索固定、长名称选择和抽屉关闭，无浏览器页面错误。临时检查页面已删除。未连接生产后端调用付费模型。

前端布局：设置按 Connection、System Prompt、Generation、Images 分区，桌面固定栏头；下拉菜单限制可见高度并支持模型名换行，手机使用设置抽屉，输入区适配安全边距。用户列表实时读取邮箱；空值显示 No email linked，已有历史空邮箱需要重新验证绑定。

部署顺序：先发布后端，再发布 linkinfra-web 前端；旧后端没有新增接口。无需数据库迁移。该列表反映路由配置，不额外推断模型是否支持聊天或视觉能力。
