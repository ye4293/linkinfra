# Homepage newsletter subscriptions

The public footer submits `POST /api/newsletter/subscribe` with JSON:

```json
{"email":"reader@example.com","language":"en","consent":true}
```

The route uses the existing critical-request rate limiter and accepts at most
4 KiB. Email addresses are validated, trimmed, lowercased, and saved in the
`newsletter_subscribers` table. The master node creates this table during the
normal startup migration. No separate mailing-service credentials are needed
to collect subscriptions.

Successful initial and duplicate submissions both return
`{"success":true,"message":"subscribed"}`. The unique email index prevents
duplicate rows; retries preserve the original consent timestamp and language.
Missing consent or invalid email returns 400. Database errors return 503.

Admins can retrieve the list using their existing authentication:
`GET /api/newsletter/subscribers?page=1&page_size=50` (maximum page size 100).
The response contains `data.items`, `data.total`, `data.page`, and
`data.page_size`. Never expose this endpoint without `AdminAuth`.

Deploy the backend before enabling the frontend form. The frontend proxies
through its existing `NEXT_PUBLIC_API_BASE_URL` setting.

This change collects explicit opt-ins; it does not send a welcome email or
schedule newsletter campaigns. Before using this list for campaigns, the
delivery integration must handle email verification and unsubscribe requests.
