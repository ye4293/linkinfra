package controller

import (
	"github.com/gin-gonic/gin"
	dbmodel "github.com/songquanpeng/one-api/model"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"time"
)

func beginSourceMetrics(c *gin.Context) func() {
	ctx, finish := dbmodel.BeginMetricsCapture(c.Request.Context(), c.GetString("original_model"))
	c.Request = c.Request.WithContext(ctx)
	return finish
}

func measureSourceAttempt(c *gin.Context, fn func() *relaymodel.ErrorWithStatusCode) *relaymodel.ErrorWithStatusCode {
	start := time.Now()
	c.Set("metrics_upstream_started", false)
	source, channel := c.GetString("metrics_source"), c.GetInt("channel_id")
	outcome := "error"
	defer func() {
		if !c.GetBool("metrics_upstream_started") {
			return
		}
		if c.Request.Context().Err() != nil {
			outcome = "cancelled"
		}
		dbmodel.RecordMetricsAttempt(c.Request.Context(), source, channel, start, outcome)
	}()
	err := fn()
	if err == nil {
		outcome = "success"
	}
	return err
}
