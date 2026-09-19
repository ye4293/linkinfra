package model

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewsletterQueueRetryLeaseAndSegmentChange(t *testing.T) {
	db := setupTestDB(t, &NewsletterSubscriber{})
	require.NoError(t, SubscribeNewsletter("reader@example.com", "en"))
	rows, err := PendingNewsletterSubscribers("segment-1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	claimed, err := ClaimNewsletterSubscriber(row.ID, "segment-1")
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = ClaimNewsletterSubscriber(row.ID, "segment-1")
	require.NoError(t, err)
	require.False(t, claimed)
	require.NoError(t, RetryNewsletterSync("segment-1"))
	claimed, _ = ClaimNewsletterSubscriber(row.ID, "segment-1")
	require.False(t, claimed, "manual retry must not steal a lease")
	require.NoError(t, CompleteNewsletterSync(row, "segment-1", "", false, errors.New("resend_http_429")))
	rows, err = PendingNewsletterSubscribers("segment-1", 10)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, db.First(&row, row.ID).Error)
	require.Equal(t, "failed", row.SyncState)
	require.Greater(t, row.NextSyncAt, time.Now().Unix())
	require.NoError(t, RetryNewsletterSync("segment-1"))
	rows, err = PendingNewsletterSubscribers("segment-1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, CompleteNewsletterSync(row, "segment-1", "contact-1", false, nil))
	claimed, _ = ClaimNewsletterSubscriber(row.ID, "segment-1")
	require.False(t, claimed, "completed rows cannot be claimed by a stale batch")
	rows, err = PendingNewsletterSubscribers("segment-2", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	stats, err := GetNewsletterSyncStats("segment-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Synced)
	require.Zero(t, stats.Pending)
}

func TestNewsletterSuppressedContactIsNotResubscribedByRetry(t *testing.T) {
	setupTestDB(t, &NewsletterSubscriber{})
	require.NoError(t, SubscribeNewsletter("reader@example.com", "en"))
	rows, _ := PendingNewsletterSubscribers("segment-1", 10)
	require.NoError(t, CompleteNewsletterSync(rows[0], "segment-1", "contact-1", true, nil))
	require.NoError(t, SubscribeNewsletter("reader@example.com", "zh"))
	require.NoError(t, RetryNewsletterSync("segment-1"))
	rows, err := PendingNewsletterSubscribers("segment-1", 10)
	require.NoError(t, err)
	require.Empty(t, rows)
	stats, err := GetNewsletterSyncStats("segment-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Suppressed)
}

func TestNewsletterMigrationQueuesExistingSubscribers(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.Exec("CREATE TABLE newsletter_subscribers (id integer primary key, email text, language text, created_at datetime)").Error)
	require.NoError(t, db.Exec("INSERT INTO newsletter_subscribers(id,email,language) VALUES (1,'reader@example.com','zh')").Error)
	require.NoError(t, db.AutoMigrate(&NewsletterSubscriber{}))
	rows, err := PendingNewsletterSubscribers("segment-1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "pending", rows[0].SyncState)
}
