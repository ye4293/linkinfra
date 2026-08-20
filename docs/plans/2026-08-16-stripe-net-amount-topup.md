# Stripe 按净收金额（扣手续费后）充值额度

## 背景与目标

当前 Stripe 充值入账额度基准是 `topUp.Amount`（下单数量），与 Stripe 实际收款及手续费无关：
- 用户充 amount=10 → Stripe 收 $10 → 到账 10 × 500000 = $10 等值额度

商家希望**按 Stripe 扣完手续费后实际净收的金额**给用户折算额度：
- 用户充 $10 → Stripe 扣手续费 $0.59 → 商家净收 $9.41 → 用户到账 $9.41 等值额度

Stripe 官方文档已确认：
- `balance_transaction.net`（integer，最小货币单位）= `amount - fee`，是扣除手续费后的净额
- 获取路径：Checkout Session → `payment_intent` → `latest_charge` → `balance_transaction` → `net`
- `checkout.session.completed` 事件里的 session 对象不含 balance_transaction，需主动调 `paymentintent.Get` 并 expand `latest_charge.balance_transaction`

## 方案设计

### 1. controller/topup_stripe.go

`stripeSessionCompleted` 改为：
1. 从 event 取 `payment_intent` ID
2. 调 `paymentintent.Get(piID, params)`，`params.AddExpand("latest_charge.balance_transaction")`
3. 取 `pi.LatestCharge.BalanceTransaction.Net`（cents）+ `.Currency`
4. 任一缺失 → 返回 error（让 Stripe 重试，不 fallback 毛额）

`StripeWebhook` 改为：`stripeSessionCompleted` 返回 error 时 `AbortWithStatus(503)`，Stripe 会重试。订单已完成的重复 webhook 由 `status != pending` 幂等早退。

去掉原 `amount_total` 解析与 `CompleteStripeTopUp`（毛额 fallback）路径。

### 2. model/topup_stripe.go

`CompleteStripeTopUpFromCheckout(tradeNo, netTotal, currency)`：参数语义从"amount_total 毛额"改为"net 净额"，内部 `netMajor := StripeAmountTotalToMajor(netTotal, currency)`，`quota := AmountToQuota(netMajor)`，传 `quotaOverride` 给 `completeTopUpOrder`。

### 3. model/topup.go

`completeTopUpOrder` 新增 `quotaOverride *int64` 参数：
- 非 nil → `quotaToAdd = *quotaOverride`（Stripe 净额路径）
- nil → `quotaToAdd = AmountToQuota(float64(topUp.Amount))`（易支付、管理员补单，行为不变）

更新 4 个调用方签名。

### 4. 货币

仅处理 USD（商家账户货币）。`balance_transaction.currency` 非 `usd` 时记录告警日志但不阻断（用户场景为 USD；触发即说明配置异常，需人工核查）。

## 影响范围

- **商业语义变更**：用户充 $10 到账额度从 $10 降为净收金额（如 $9.41），手续费由用户额度承担。
- **可靠性**：卡支付（同步）下 balance_transaction 在 `checkout.session.completed` 时已就绪；异步支付方式极端情况下拿不到时返回 503 重试。
- **不影响**：易支付、管理员补单、邀请返现、等级判定逻辑。
- **无 schema 变更**：`TopUp.Money` 改为记录净额，`Currency` 记录结算货币。

## 验证方式

- `go build ./... && go vet ./...`
- 充值流程：充 $10 → Stripe 收 $10 扣手续费 → webhook 拿 net → 到账 `net × 500000` quota = 净额等值
- 重复 webhook 不重复加额度（幂等）
