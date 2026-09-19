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
	ID        int       `json:"id" gorm:"primaryKey"`
	Email     string    `json:"email" gorm:"size:254;uniqueIndex;not null"`
	Language  string    `json:"language" gorm:"size:2;not null"`
	CreatedAt time.Time `json:"created_at"`
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
