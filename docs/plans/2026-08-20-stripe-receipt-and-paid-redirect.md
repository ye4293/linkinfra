# Stripe 充值：支付成功反馈 + 交易记录收据链接

## 背景与目标

实测生产 $10 支付宝充值（session cs_live_b1m4JVlz..., charge py_3U6Spq...）：
- 链路正常：status complete/paid，balance_transaction.net=941（到账 4,705,000 quota ✅）
- Stripe 提供 receipt_url（https://pay.stripe.com/receipts/payment/...），但当前未存、未展示

两个问题：
1. **支付后无成功反馈**：前端 `window.open` 新 tab 支付，Stripe 跳 success_url 回 `/dashboard/topup`，但该页不检测回跳参数，不弹"成功"、不刷新余额/记录。支付宝异步 processing 卡住是 Stripe 端（等回调），前端改不了；但"跳回后无反馈"能改。
2. **交易记录页缺 Stripe 收据链接**：`TopUp` 表无 receipt_url 字段，`transaction-history.tsx` 无收据列。

## 方案设计

### 后端（linkinfra）

1. `model/topup.go`
   - `TopUp` 加字段 `ReceiptUrl string`（GORM AutoMigrate 加列，master 重启触发，varchar(512)）
   - `completeTopUpOrder` 加 `receiptOverride *string` 参数，非 nil 时写入 `topUp.ReceiptUrl`
2. `model/topup_stripe.go`：`CompleteStripeTopUpFromCheckout` 加 `receiptUrl string` 参数，传 `receiptOverride`
3. `controller/topup_stripe.go`
   - `fetchStripeNetAmount` 扩展返回 `receiptUrl`（`pi.LatestCharge.ReceiptUrl`，charge 已 expand）
   - `stripeSessionCompleted` 取 receiptUrl 传给 model
   - `genStripeCheckoutLink`：success_url 拼 `?paid=1`（Stripe 跳转时附加 session_id，前端读 paid）
4. `model/topup_stripe_net_test.go`：更新 `CompleteStripeTopUpFromCheckout` 调用签名

### 前端（linkinfra-web）

1. `app/dashboard/topup/page.tsx`：page 接收 `searchParams`，传 `paid` 给 `TopupPageView`
2. `sections/topup/view/topupPage.tsx`：`TopupPage` 接收 `paid` prop，渲染 `<PaymentSuccessIndicator paid={paid} />`
3. `sections/topup/payment-success-indicator.tsx`（新建，client）：`useEffect` 检测 paid → `toast.success("充值成功")` + `router.refresh()` + 去掉 URL 的 `?paid`（避免刷新重复弹）
4. `sections/topup/transaction-history.tsx`：`TopUpRecord` 加 `receipt_url?`，表格加「Receipt」列，success 且有 receipt_url 的显示「View receipt」外链

## 影响范围

- DB：TopUp 加 `receipt_url` 列（AutoMigrate，生产 master 重启触发；ALTER TABLE 加列，符合规范）
- 不影响 webhook 入账/净额/邀请返现逻辑
- 支付宝 processing 卡住本身不改（Stripe 端等回调）；仅改跳回后反馈

## 验证

- `go build ./... && go vet ./... && go test ./model/ ./controller/`
- 前端 `npx tsc --noEmit`
- 真实充值：支付宝付 $1 → 跳回 `/dashboard/topup?paid=1` → 弹成功 toast + 余额刷新 + 交易记录出现「View receipt」链接
