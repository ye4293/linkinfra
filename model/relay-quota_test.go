package model

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRelayQuotaConcurrentReservations(t *testing.T) {
	db := setupTestDB(t, &User{}, &Token{}, &Channel{})
	oldBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = true
	t.Cleanup(func() { config.BatchUpdateEnabled = oldBatch })
	require.NoError(t, db.Create(&User{Id: 1, Username: "quota", Quota: 100}).Error)
	require.NoError(t, db.Create(&Token{Id: 1, UserId: 1, Key: "quota", RemainQuota: 100}).Error)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if AdjustRelayQuota(1, 1, 10, true) == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 10, successes.Load())
	var user User
	var token Token
	require.NoError(t, db.First(&user, 1).Error)
	require.NoError(t, db.First(&token, 1).Error)
	require.Zero(t, user.Quota)
	require.Zero(t, token.RemainQuota)
	require.EqualValues(t, 100, token.UsedQuota)
}

func TestRelayQuotaRefundRestoresExhaustedToken(t *testing.T) {
	db := setupTestDB(t, &User{}, &Token{})
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })
	require.NoError(t, db.Create(&User{Id: 1, Username: "refund", Quota: 100}).Error)
	require.NoError(t, db.Create(&Token{Id: 1, UserId: 1, Key: "refund", Status: common.TokenStatusEnabled, RemainQuota: 100}).Error)
	require.NoError(t, AdjustRelayQuota(1, 1, 100, true))
	_, err := ValidateUserToken("refund")
	require.Error(t, err)
	var token Token
	require.NoError(t, db.First(&token, 1).Error)
	require.Equal(t, common.TokenStatusExhausted, token.Status)
	require.NoError(t, AdjustRelayQuota(1, 1, -100, false))
	_, err = ValidateUserToken("refund")
	require.NoError(t, err)
	// 退款不允许重新启用管理员禁用的令牌。
	require.NoError(t, db.Model(&Token{}).Where("id = ?", 1).Update("status", common.TokenStatusDisabled).Error)
	require.NoError(t, AdjustRelayQuota(1, 1, -1, false))
	_, err = ValidateUserToken("refund")
	require.Error(t, err)
}

func TestRelayQuotaRefundDuringTokenValidation(t *testing.T) {
	db := setupTestDB(t, &User{}, &Token{})
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })
	require.NoError(t, db.Create(&User{Id: 1, Username: "refund-race", Quota: 100}).Error)
	require.NoError(t, db.Create(&Token{Id: 1, UserId: 1, Key: "refund-race", Status: common.TokenStatusEnabled, RemainQuota: 100}).Error)
	require.NoError(t, AdjustRelayQuota(1, 1, 100, true))
	refunded := false
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("refund_after_read", func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" && !refunded {
			refunded = true
			require.NoError(t, AdjustRelayQuota(1, 1, -100, false))
		}
	}))
	_, _ = ValidateUserToken("refund-race")
	_, err := ValidateUserToken("refund-race")
	require.NoError(t, err, "stale exhausted snapshot must not disable refunded token")
}
