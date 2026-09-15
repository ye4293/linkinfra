package model

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// AdjustAudioQuota moves user and token balances together. Reservations enforce
// available balances; settlement applies the actual cost minus the reservation.
// A negative delta refunds both balances exactly once at the caller.
func AdjustAudioQuota(userID, tokenID int, delta int64, reserve bool) error {
	if reserve && delta < 0 {
		return fmt.Errorf("audio reservation cannot be negative")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := tx.Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
			return err
		}
		if delta == 0 {
			return nil
		}
		userUpdate := tx.Model(&User{}).Where("id = ?", userID)
		if reserve {
			userUpdate = userUpdate.Where("quota >= ?", delta)
		}
		result := userUpdate.Update("quota", gorm.Expr("quota - ?", delta))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("insufficient user quota")
		}
		tokenUpdate := tx.Model(&Token{}).Where("id = ? AND user_id = ?", tokenID, userID)
		if reserve && !token.UnlimitedQuota {
			tokenUpdate = tokenUpdate.Where("remain_quota >= ?", delta)
		}
		result = tokenUpdate.Updates(map[string]interface{}{
			"remain_quota":  gorm.Expr("remain_quota - ?", delta),
			"used_quota":    gorm.Expr("used_quota + ?", delta),
			"accessed_time": time.Now().Unix(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("insufficient token quota")
		}
		return nil
	})
}
