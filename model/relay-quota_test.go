package model

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/stretchr/testify/require"
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
