# Newsletter subscriptions and Resend Broadcasts

## Set up

1. Deploy the backend before the updated frontend. The master node migrates
   `newsletter_subscribers` and runs the synchronization worker.
2. In **System settings → Configure Resend**, save a Resend API key with
   **Full access**. Sending-only keys cannot manage contacts or segments.
   Keep the sender domain verified in Resend.
3. In **Newsletter & broadcasts**, click **Create segment**, or enter an existing
   Segment ID and save it. This is root-only. The backend validates existing
   segments against Resend before saving `ResendNewsletterSegmentId`.
4. Existing and new explicit homepage subscriptions synchronize automatically.
   Check the pending/failed counts and use **Retry pending sync** if needed.

The frontend uses its existing `NEXT_PUBLIC_API_BASE_URL` proxy. API keys remain
on the backend. Use a dedicated newsletter segment, not one with account emails.

## Send a campaign

Click **Open Resend Broadcasts** in system settings. In Resend:

1. Create a Broadcast and choose the configured newsletter segment.
2. Choose a sender at your verified domain; write the subject and content.
3. Add Resend's unsubscribe footer. Resend excludes unsubscribed contacts from
   subsequent Broadcasts.
4. Preview/test, then send or schedule the message.

Editing, scheduling, delivery statistics and unsubscribe handling use Resend's
native dashboard, rather than a separate campaign composer in this application.
Contact synchronization does not send a welcome email or a campaign.
Resend account access and contacts/broadcast plan limits still apply.

## Persistence, synchronization and opt-outs

`POST /api/newsletter/subscribe` accepts:

```json
{"email":"reader@example.com","language":"en","consent":true}
```

Email validation, the 4 KiB body limit, critical-request rate limiting and the
unique email index apply. Success means the signup is durably stored locally,
not that Resend has already accepted the contact. No account email is enrolled
automatically. This is a single-opt-in signup; it does not send verification mail.

The worker wakes on local signups/configuration/retries and polls every 15 seconds,
processing up to 10 records per batch. Database leases protect concurrent workers.
Resend requests are spaced by 600 ms per process. Failed syncs remain durable and
retry with exponential backoff from one minute to one hour. Deploy one master;
other nodes' signups are discovered by the master's next poll.

Synchronization reads an existing contact's Resend opt-out state and adds eligible
contacts to the segment. It never PATCHes contacts or sends `unsubscribed:false`.
An opted-out contact is marked `suppressed` locally; repeated public signups and
manual sync retries cannot re-enable it. Preference changes happen in Resend.
Resend remains authoritative for current eligibility: local synced counts describe
sync history, not a live recipient count.

When the configured segment changes, records are queued for the new segment.
Old memberships are not removed automatically. Do not send to an obsolete segment.

## Administrative endpoints

- `GET /api/newsletter/status`: configuration readiness, segment ID, sync counts (admin).
- `GET /api/newsletter/subscribers?page=1&page_size=50`: stored records and sync
  metadata (admin, maximum page size 100). `sync_error` contains a sanitized code.
  Common codes: `resend_http_401`/`403` (API key/permissions), `resend_http_404`
  (missing segment), `resend_http_429` (rate limit), and `resend_unavailable`.
- `POST /api/newsletter/sync`: queue pending/failed records immediately (admin).
- `POST /api/newsletter/config`: `{"create":true}` or
  `{"segment_id":"UUID"}`; validate and save a segment (root).

## API references

- https://resend.com/docs/api-reference/contacts/create-contact
- https://resend.com/docs/api-reference/contacts/get-contact
- https://resend.com/docs/api-reference/contacts/add-contact-to-segment
- https://resend.com/docs/api-reference/segments/create-segment
- https://resend.com/docs/dashboard/broadcasts/editor
