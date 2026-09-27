package controller

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type systemOneTransport func(*http.Request) (*http.Response, error)

func (f systemOneTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func systemOneReviewFixture(t *testing.T) (*gorm.DB, *gin.Context) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
	oldDB, oldLogs, oldClient := model.DB, model.LOG_DB, util.HTTPClient
	oldBatch, oldRedis, oldConsume := config.BatchUpdateEnabled, common.RedisEnabled, config.LogConsumeEnabled
	oldPrices, oldGroups := common.ModelPrice, common.GroupRatio
	oldCompletion := common.CompletionRatio["jev-latest"]
	t.Cleanup(func() {
		model.DB, model.LOG_DB, util.HTTPClient = oldDB, oldLogs, oldClient
		config.BatchUpdateEnabled, common.RedisEnabled, config.LogConsumeEnabled = oldBatch, oldRedis, oldConsume
		common.ModelPrice, common.GroupRatio = oldPrices, oldGroups
		common.CompletionRatio["jev-latest"] = oldCompletion
		sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	config.BatchUpdateEnabled, common.RedisEnabled, config.LogConsumeEnabled = false, false, true
	common.ModelPrice, common.GroupRatio = map[string]float64{}, map[string]float64{"review": 1}
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "review", Quota: 100000}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 1, UserId: 1, Key: "review", RemainQuota: 100000}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 1, Name: "review"}).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/systemone", strings.NewReader(strings.Replace(systemOneTestRequest, "test-alias", "jev-latest", 1)))
	c.Set("id", 1)
	c.Set("token_id", 1)
	c.Set("channel_id", 1)
	c.Set("channel", common.ChannelTypeCustom)
	c.Set("base_url", "https://typesafe.test")
	c.Set("group", "review")
	c.Set("actual_key", "test-key")
	return db, c
}

func systemOneReviewResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestSystemOneReviewTariffFrozen(t *testing.T) {
	for _, change := range []string{"fixed", "output"} {
		t.Run(change, func(t *testing.T) {
			db, c := systemOneReviewFixture(t)
			util.HTTPClient = &http.Client{Transport: systemOneTransport(func(*http.Request) (*http.Response, error) {
				if change == "fixed" {
					common.ModelPrice["jev-latest"] = 0.5
				} else {
					common.CompletionRatio["jev-latest"] = 100
				}
				return systemOneReviewResponse(systemOneTestResponse), nil
			})}
			require.Nil(t, RelaySystemOneHelper(c))
			var user model.User
			require.NoError(t, db.First(&user, 1).Error)
			require.EqualValues(t, 99979, user.Quota)
		})
	}
}

func TestSystemOneReviewReservationRollback(t *testing.T) {
	db, c := systemOneReviewFixture(t)
	util.HTTPClient = &http.Client{Transport: systemOneTransport(func(*http.Request) (*http.Response, error) { t.Fatal("upstream must not be called"); return nil, nil })}
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("fail_user", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("injected user update failure"))
		}
	}))
	require.NotNil(t, RelaySystemOneHelper(c))
	var token model.Token
	require.NoError(t, db.First(&token, 1).Error)
	require.EqualValues(t, 100000, token.RemainQuota)
}

func TestSystemOneReviewSettlementRollback(t *testing.T) {
	db, c := systemOneReviewFixture(t)
	util.HTTPClient = &http.Client{Transport: systemOneTransport(func(*http.Request) (*http.Response, error) {
		failed := false
		require.NoError(t, db.Callback().Update().Before("gorm:update").Register("fail_settlement", func(tx *gorm.DB) {
			if !failed && tx.Statement.Table == "tokens" {
				failed = true
				tx.AddError(errors.New("injected settlement failure"))
			}
		}))
		return systemOneReviewResponse(systemOneTestResponse), nil
	})}
	require.NotNil(t, RelaySystemOneHelper(c), "settlement failure must not return success")
	require.True(t, c.GetBool("systemone_no_retry"))
	var user model.User
	var token model.Token
	require.NoError(t, db.First(&user, 1).Error)
	require.NoError(t, db.First(&token, 1).Error)
	require.EqualValues(t, 100000, user.Quota)
	require.EqualValues(t, 100000, token.RemainQuota)
	var count int64
	require.NoError(t, db.Model(&model.Log{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestSystemOneReviewRejectsIncompleteAnswers(t *testing.T) {
	for _, body := range []string{
		`{"model":"jev-1.13.0","answers":{"check":null},"usage":{"input_tokens":1000,"output_tokens":20}}`,
		`{"model":"jev-1.13.0","answers":{"check":{"type":"noul","noul":0.99}},"usage":{"input_tokens":1000,"output_tokens":20}}`,
	} {
		t.Run(body, func(t *testing.T) {
			db, c := systemOneReviewFixture(t)
			util.HTTPClient = &http.Client{Transport: systemOneTransport(func(*http.Request) (*http.Response, error) { return systemOneReviewResponse(body), nil })}
			require.NotNil(t, RelaySystemOneHelper(c))
			var user model.User
			require.NoError(t, db.First(&user, 1).Error)
			require.EqualValues(t, 100000, user.Quota)
		})
	}
}

func TestSystemOneReviewNonJSONError(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{502, "<html>bad gateway</html>", 502}, {204, "", 502}, {201, `{"detail":"unexpected response"}`, 502}, {429, `{"detail":""}`, 429},
		{304, "", 502}, {302, `{"detail":"unexpected redirect"}`, 502},
	} {
		err := systemOneError(&http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))})
		require.NotEmpty(t, err.Error.Message)
		require.Equal(t, tc.want, err.StatusCode)
	}
}

func TestSystemOneReviewBatchAndZeroUsage(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(map[bool]string{false: "batch", true: "zero usage"}[zero], func(t *testing.T) {
			db, c := systemOneReviewFixture(t)
			config.BatchUpdateEnabled = true
			util.HTTPClient = &http.Client{Transport: systemOneTransport(func(*http.Request) (*http.Response, error) {
				var user model.User
				var token model.Token
				require.NoError(t, db.First(&user, 1).Error)
				require.NoError(t, db.First(&token, 1).Error)
				require.Less(t, user.Quota, int64(100000))
				require.Equal(t, user.Quota, token.RemainQuota)
				body := systemOneTestResponse
				if zero {
					body = strings.Replace(body, `"input_tokens":1000,"output_tokens":20`, `"input_tokens":0,"output_tokens":0`, 1)
				}
				return systemOneReviewResponse(body), nil
			})}
			require.Nil(t, RelaySystemOneHelper(c))
			quota := int64(21)
			if zero {
				quota = 0
			}
			var user model.User
			var token model.Token
			var ch model.Channel
			var logs []model.Log
			require.NoError(t, db.First(&user, 1).Error)
			require.NoError(t, db.First(&token, 1).Error)
			require.NoError(t, db.First(&ch, 1).Error)
			require.EqualValues(t, 100000-quota, user.Quota)
			require.Equal(t, user.Quota, token.RemainQuota)
			require.Equal(t, quota, ch.UsedQuota)
			require.Equal(t, quota, user.UsedQuota)
			require.EqualValues(t, 1, user.RequestCount)
			require.NoError(t, db.Find(&logs).Error)
			require.Len(t, logs, 1)
			require.EqualValues(t, quota, logs[0].Quota)
		})
	}
}

func TestSystemOneReviewSettlementStatsRollback(t *testing.T) {
	db, c := systemOneReviewFixture(t)
	util.HTTPClient = &http.Client{Transport: systemOneTransport(func(*http.Request) (*http.Response, error) {
		require.NoError(t, db.Callback().Update().Before("gorm:update").Register("fail_channel", func(tx *gorm.DB) {
			if tx.Statement.Table == "channels" {
				tx.AddError(errors.New("channel update failed"))
			}
		}))
		return systemOneReviewResponse(systemOneTestResponse), nil
	})}
	require.NotNil(t, RelaySystemOneHelper(c))
	var user model.User
	var token model.Token
	require.NoError(t, db.First(&user, 1).Error)
	require.NoError(t, db.First(&token, 1).Error)
	require.EqualValues(t, 100000, user.Quota)
	require.EqualValues(t, 100000, token.RemainQuota)
	require.Zero(t, user.UsedQuota)
	require.Zero(t, user.RequestCount)
	require.Zero(t, token.UsedQuota)
}

func TestSystemOneReviewDecimalBilling(t *testing.T) {
	_, c := systemOneReviewFixture(t)
	meta := util.GetRelayMeta(c)
	meta.ActualModelName = "jev-latest"
	tariff, err := newSystemOneTariff(meta)
	require.NoError(t, err)
	quota, err := tariff.quota(3000, 0)
	require.NoError(t, err)
	require.EqualValues(t, 63, quota)
	meta.ChannelDiscount = 0.8
	meta.UserChannelRatio = 0.9
	tariff, err = newSystemOneTariff(meta)
	require.NoError(t, err)
	quota, err = tariff.quota(100000, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1512, quota)
}
