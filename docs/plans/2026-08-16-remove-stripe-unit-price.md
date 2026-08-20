# 移除 Stripe `StripeUnitPrice` 配置

## 背景与目标

`StripeUnitPrice` 在当前链路中只剩"前端预览金额展示"作用，既不影响实际收款（由 `StripePriceId` 单价 × quantity 决定），也不影响入账额度（由 `amount × QuotaPerUnit` 决定）。

已确认 Stripe 后台 `StripePriceId` 单价为 **$1/unit**，即 `amount` 本身就是美金数量：

- 用户点 amount=10 → Stripe 实付 $10 → 到账 `10 × 500000 = 5,000,000 quota` = $10 等值额度
- 付款金额与到账额度天然对等，`StripeUnitPrice` 完全冗余

目标：前后端彻底删除 `StripeUnitPrice` 配置及其依赖的预览接口 `/api/user/stripe/amount`，前端 "You pay" 直接显示 `amount`。

## 方案设计

### 后端（linkinfra）
1. `common/config/config.go` — 删 `var StripeUnitPrice = 8.0`
2. `model/option.go` — 删 `OptionMap["StripeUnitPrice"]` 初始化与 `case "StripeUnitPrice"` 分支
3. `controller/topup_stripe.go`：
   - 删 `getStripePayMoney`
   - 删 `RequestStripeAmount`（预览接口失去依据）
   - `RequestStripePay` 中删 `payMoney` 计算与 `payMoney < 0.01` 校验
4. `model/topup_stripe.go` — `CreateStripeTopUp` 去掉 `money` 参数，内部 `Money: float64(amount)`（$1/unit 下即美金；webhook 回调仍会用真实 `amount_total` 覆盖）
5. `router/api-router.go` — 删 `POST /stripe/amount` 路由

### 前端（linkinfra-web）
1. `sections/setting/view/paymentSettingPage.tsx` — 删 `stripeUnitPrice` state、读取、保存项、表单字段
2. `sections/topup/payment-section.tsx` — 删除调 `/api/user/stripe/amount` 的 useEffect，`payAmount` 直接取 `amount`
3. `app/api/user/stripe/amount/route.ts` — 删除整个文件

## 影响范围

- 已存在的 `options` 表里若残留 `StripeUnitPrice` 行：不影响运行（`updateOptions` 不再有对应 case，该 key 会被忽略）。无需数据迁移。
- 前端充值页 "You pay" 由后端预算改为本地 `amount` 直显，行为等价（$1/unit）。
- 不影响 webhook 入账、额度结算、邀请返现等既有逻辑。

## 验证方式

- `go build ./... && go vet ./...`
- 前端 `payment-section.tsx` 与 `paymentSettingPage.tsx` 无残留引用、类型检查通过
- 充值流程：点 10 → "You pay: $10" → 跳 Stripe 收 $10 → 回调入账 5,000,000 quota
