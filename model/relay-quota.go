package model

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

var ErrInsufficientRelayQuota = errors.New("insufficient user or token quota")

// AdjustRelayQuota 即时事务更新两种余额，预扣使用条件 UPDATE 避免并发透支。
func AdjustRelayQuota(userID, tokenID int, delta int64, reserve bool) error {
	if reserve && delta < 0 {
		return fmt.Errorf("reservation cannot be negative")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		return adjustRelayQuota(tx, userID, tokenID, delta, reserve)
	})
}

func adjustRelayQuota(tx *gorm.DB, userID, tokenID int, delta int64, reserve bool) error {
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
		return ErrInsufficientRelayQuota
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
		return ErrInsufficientRelayQuota
	}
	return nil
}

// SettleRelayQuota 将实际扣费、用户用量和渠道用量放在同一事务，失败全部回滚。
func SettleRelayQuota(userID, tokenID, channelID int, reserved, quota int64) error {
	if reserved < 0 || quota < 0 {
		return fmt.Errorf("quota cannot be negative")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := adjustRelayQuota(tx, userID, tokenID, quota-reserved, false); err != nil {
			return err
		}
		result := tx.Model(&User{}).Where("id = ?", userID).Updates(map[string]interface{}{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"request_count": gorm.Expr("request_count + 1"),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("user missing during settlement")
		}
		// 免费请求避免相同值 UPDATE 在 MySQL 中返回零行。
		if quota == 0 {
			return nil
		}
		result = tx.Model(&Channel{}).Where("id = ?", channelID).Update("used_quota", gorm.Expr("used_quota + ?", quota))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("channel missing during settlement")
		}
		return nil
	})
}
