package model

import (
	"errors"
	"net/mail"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

var ErrInvalidNewsletterEmail = errors.New("invalid newsletter email")

// NewsletterSubscriber records explicit consent from the public signup form.
// Registration and account email addresses are never added automatically.
type NewsletterSubscriber struct {
	ID              int       `json:"id" gorm:"primaryKey"`
	Email           string    `json:"email" gorm:"size:254;uniqueIndex;not null"`
	Language        string    `json:"language" gorm:"size:2;not null"`
	CreatedAt       time.Time `json:"created_at"`
	ResendContactID string    `json:"resend_contact_id" gorm:"size:64;not null;default:''"`
	SyncedSegmentID string    `json:"synced_segment_id" gorm:"size:64;not null;default:''"`
	SyncState       string    `json:"sync_state" gorm:"size:20;not null;default:'pending'"`
	SyncError       string    `json:"sync_error" gorm:"size:100;not null;default:''"`
	SyncAttempts    int       `json:"sync_attempts" gorm:"not null;default:0"`
	NextSyncAt      int64     `json:"next_sync_at" gorm:"not null;default:0;index"`
	LeaseUntil      int64     `json:"-" gorm:"not null;default:0"`
}

func SubscribeNewsletter(email, language string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || len(email) > 254 || address.Address != email {
		return ErrInvalidNewsletterEmail
	}
	if language != "zh" {
		language = "en"
	}
	subscriber := NewsletterSubscriber{Email: email, Language: language}
	// The unique index makes retries and concurrent submissions idempotent.
	// Keep the original consent timestamp and preference on duplicate requests.
	return DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&subscriber).Error
}

func ListNewsletterSubscribers(page, pageSize int) ([]NewsletterSubscriber, int64, error) {
	subscribers := make([]NewsletterSubscriber, 0)
	var total int64
	if err := DB.Model(&NewsletterSubscriber{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := DB.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&subscribers).Error
	return subscribers, total, err
}
