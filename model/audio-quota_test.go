package model

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAudioReservationRollsBackWhenTokenBalanceIsInsufficient(t *testing.T) {
	db := setupTestDB(t, &User{}, &Token{})
	require.NoError(t, db.Create(&User{Id: 9301, Username: "audioquota", Quota: 10000}).Error)
	require.NoError(t, db.Create(&Token{Id: 9302, UserId: 9301, Key: "audio-quota-test", RemainQuota: 100}).Error)
	require.Error(t, AdjustAudioQuota(9301, 9302, 2250, true))
	var user User
	var token Token
	require.NoError(t, db.First(&user, 9301).Error)
	require.NoError(t, db.First(&token, 9302).Error)
	assert.EqualValues(t, 10000, user.Quota)
	assert.EqualValues(t, 100, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	require.NoError(t, AdjustAudioQuota(9301, 9302, 100, true))
	require.NoError(t, AdjustAudioQuota(9301, 9302, -62, false))
	require.NoError(t, db.First(&user, 9301).Error)
	require.NoError(t, db.First(&token, 9302).Error)
	assert.EqualValues(t, 9962, user.Quota)
	assert.EqualValues(t, 62, token.RemainQuota)
	assert.EqualValues(t, 38, token.UsedQuota)
}
