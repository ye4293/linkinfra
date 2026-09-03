# 邮件发送迁移到 Resend，移除 SMTP

## 背景与目标

当前系统邮件（注册验证码、重置密码、额度提醒、订单通知、管理员告警、测试邮件）全部通过 `common/message/email.go` 的 `SendEmail` 走 SMTP 发送，需要在系统设置里维护服务器、端口、账号、凭证等多项配置，且 SMTP 在部分部署环境下端口受限、投递率不稳定。

目标：后端改用 [Resend](https://resend.com/docs/introduction) HTTP API 发信，彻底移除 SMTP 配置与实现；前端设置页对应替换为 Resend 配置卡片。`SendEmail(subject, receiver, content)` 签名保持不变，7 处业务调用点无需修改。

## 方案设计

### 后端（linkinfra）

| 文件 | 改动 |
| --- | --- |
| `common/config/config.go` | 删除 `SMTPServer/SMTPPort/SMTPAccount/SMTPFrom/SMTPToken`，新增 `ResendApiKey`、`ResendFrom` |
| `model/option.go` | `InitOptionMap` 删除 5 个 SMTP 默认项，新增 `ResendApiKey`、`ResendFrom`（默认空）；`updateOptionMap` 替换对应分支 |
| `controller/option.go` | `GetOptions` 脱敏规则增加 `ApiKey` 后缀，确保 `ResendApiKey` 不回显 |
| `common/message/email.go` | 重写为 Resend 实现：`POST {resendBaseURL}/emails`，Bearer 认证，JSON 体 `{from, to[], subject, html}`；`from` 为 `SystemName <ResendFrom>`；主题加 `[SystemName]` 前缀；`receiver` 按 `;` 拆分为 `to` 数组；未配置 key 或发件地址时返回 `resend is not configured`；非 2xx 时把响应中的 `message` 带入错误；超时 15 秒。`resendBaseURL` 为包内变量，便于测试替换 |
| `common/message/email_test.go` | 新增：用 `httptest.Server` 覆盖成功、未配置、Resend 返回 4xx 三种情况 |
| `controller/notification.go` | `TestSMTP` 改为 `TestEmail`，检查 Resend 配置，测试邮件文案改为 Resend |
| `router/api-router.go` | `/api/test/smtp` 改为 `/api/test/email` |

不采用官方 Go SDK：Resend 发信只有一个端点，直接 `net/http` 调用约 40 行，避免新增依赖。

### 前端（linkinfra-web）

| 文件 | 改动 |
| --- | --- |
| `sections/setting/view/settingPage.tsx` | SMTP 卡片替换为 "Configure Resend" 卡片，字段仅 API key（password 输入，为空不提交）与 Sender address；保存与测试按钮保留，测试按钮在发件地址为空时禁用；调用 `/api/test/email` |
| `lib/types/systemSettings.ts` | 删除 `SMTP*` 字段，新增 `ResendApiKey`、`ResendFrom` |
| `app/api/test/smtp/route.ts` | 重命名为 `app/api/test/email/route.ts`，endpoint 改为 `/api/test/email` |

前端文案全部英文，遵循项目惯例。

## 影响范围

- 所有发信场景在部署后需在设置页填写 Resend API key 与发件地址（发件域名需已在 Resend 验证），否则验证码、重置密码等会返回 "Resend is not configured"。
- `options` 表中旧的 `SMTP*` 行会残留，后端不再读取，无害，不做删除。
- 前端原来还会写入一个后端从未使用的 `SMTPSSLEnabled` 选项，一并移除。
- 无数据库 schema 变更。

## 验证方式

- 后端：`go build ./... && go vet ./... && go test ./common/message/...`
- 前端：`pnpm build`
- 手动：设置页保存 Resend 配置后点击 "Send test email"，收件箱收到测试邮件；注册流程验证码邮件可正常收到。
