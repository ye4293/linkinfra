package model

import (
	"time"

	"gorm.io/gorm"
)

func newsletterPending(db *gorm.DB, segment string) *gorm.DB {
	return db.Model(&NewsletterSubscriber{}).Where("synced_segment_id <> ? OR sync_state NOT IN ?", segment, []string{"synced", "suppressed"})
}

func PendingNewsletterSubscribers(segment string, limit int) ([]NewsletterSubscriber, error) {
	var rows []NewsletterSubscriber
	now := time.Now().Unix()
	err := newsletterPending(DB, segment).Where("next_sync_at <= ? AND lease_until <= ?", now, now).Order("id asc").Limit(limit).Find(&rows).Error
	return rows, err
}

func ClaimNewsletterSubscriber(id int, segment string) (bool, error) {
	now := time.Now().Unix()
	result := newsletterPending(DB, segment).Where("id = ? AND lease_until <= ? AND next_sync_at <= ?", id, now, now).Update("lease_until", now+120)
	return result.RowsAffected == 1, result.Error
}

func CompleteNewsletterSync(row NewsletterSubscriber, segment, contactID string, suppressed bool, syncErr error) error {
	values := map[string]interface{}{"lease_until": 0}
	if syncErr != nil {
		delay := time.Minute
		for i := 0; i < row.SyncAttempts && delay < time.Hour; i++ {
			delay *= 2
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		values["sync_state"] = "failed"
		values["sync_error"] = syncErr.Error()
		values["sync_attempts"] = row.SyncAttempts + 1
		values["next_sync_at"] = time.Now().Add(delay).Unix()
	} else {
		state := "synced"
		if suppressed {
			state = "suppressed"
		}
		values["sync_state"], values["sync_error"] = state, ""
		values["resend_contact_id"], values["synced_segment_id"] = contactID, segment
		values["sync_attempts"], values["next_sync_at"] = 0, 0
	}
	return DB.Model(&NewsletterSubscriber{}).Where("id = ?", row.ID).Updates(values).Error
}

type NewsletterSyncStats struct {
	Total      int64 `json:"total"`
	Synced     int64 `json:"synced"`
	Suppressed int64 `json:"suppressed"`
	Pending    int64 `json:"pending"`
	Failed     int64 `json:"failed"`
}

func GetNewsletterSyncStats(segment string) (NewsletterSyncStats, error) {
	var stats NewsletterSyncStats
	queries := []struct {
		query *gorm.DB
		count *int64
	}{
		{DB.Model(&NewsletterSubscriber{}), &stats.Total},
		{DB.Model(&NewsletterSubscriber{}).Where("synced_segment_id = ? AND sync_state = ?", segment, "synced"), &stats.Synced},
		{DB.Model(&NewsletterSubscriber{}).Where("synced_segment_id = ? AND sync_state = ?", segment, "suppressed"), &stats.Suppressed},
		{newsletterPending(DB, segment), &stats.Pending},
		{DB.Model(&NewsletterSubscriber{}).Where("sync_state = ?", "failed"), &stats.Failed},
	}
	for _, q := range queries {
		if err := q.query.Count(q.count).Error; err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func RetryNewsletterSync(segment string) error {
	return newsletterPending(DB, segment).Where("lease_until <= ?", time.Now().Unix()).Update("next_sync_at", 0).Error
}
