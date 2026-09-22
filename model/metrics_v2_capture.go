package model

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common/config"
)

// MetricsAttempt 不保存正文、错误原文或密钥。来源在选渠时冻结。
type MetricsAttempt struct {
	Source     string  `json:"source"`
	Channel    int     `json:"channel"`
	Outcome    string  `json:"outcome"`
	Duration   float64 `json:"duration"`
	Offset     float64 `json:"offset"`
	TTFT       float64 `json:"ttft,omitempty"`
	Prompt     int64   `json:"prompt,omitempty"`
	Completion int64   `json:"completion,omitempty"`
	UsageKnown bool    `json:"usage_known"`
}

type metricsCaptureKey struct{}
type metricsPendingLog struct {
	log   *Log
	write func(*Log)
}
type MetricsCapture struct {
	sync.Mutex
	start    time.Time
	model    string
	attempts []MetricsAttempt
	pending  []metricsPendingLog
	finished bool
	claimed  bool
	overflow bool
}

func BeginMetricsCapture(ctx context.Context, models ...string) (context.Context, func()) {
	if !config.ModelMetricsV2Enabled || !config.ModelMetricsEnabled || !config.LogConsumeEnabled {
		return ctx, func() {}
	}
	c := &MetricsCapture{start: time.Now()}
	if len(models) > 0 {
		c.model = models[0]
	}
	return context.WithValue(ctx, metricsCaptureKey{}, c), c.finish
}

func RecordMetricsAttempt(ctx context.Context, source string, channel int, start time.Time, outcome string) {
	c, _ := ctx.Value(metricsCaptureKey{}).(*MetricsCapture)
	if c == nil {
		return
	}
	c.Lock()
	defer c.Unlock()
	if len(c.attempts) >= 64 {
		c.overflow = true
		return
	}
	c.attempts = append(c.attempts, MetricsAttempt{Source: strings.ToLower(strings.TrimSpace(source)), Channel: channel, Outcome: outcome, Duration: time.Since(start).Seconds(), Offset: start.Sub(c.start).Seconds()})
}

// 成功日志可能由异步结算先于/晚于 relay 返回写入。只在请求结果冻结后补充监控字段，
// 保留原来的单次日志 INSERT；不让计费日志等待另一条监控 SQL。
func writeMetricsLog(ctx context.Context, log *Log, write func(*Log)) {
	c, _ := ctx.Value(metricsCaptureKey{}).(*MetricsCapture)
	if c == nil {
		write(log)
		return
	}
	c.Lock()
	if !c.finished && len(c.pending) < 4 {
		c.pending = append(c.pending, metricsPendingLog{log, write})
		c.Unlock()
		return
	}
	if !c.finished {
		c.overflow = true
	}
	c.enrich(log)
	c.Unlock()
	write(log)
}

func (c *MetricsCapture) enrich(log *Log) {
	if c.claimed || len(c.attempts) == 0 {
		return
	}
	last := c.attempts[len(c.attempts)-1]
	if log.ChannelId != last.Channel && !c.overflow {
		return
	}
	// 失败路径有时记录扣费日志和最终错误日志，仅一条携带监控样本。
	c.claimed = true
	log.MetricsVersion = 2
	log.MetricsModelName = c.model
	if log.MetricsModelName == "" {
		log.MetricsModelName = log.ModelName
	}
	log.MetricsSourceKey = last.Source
	// 预算溢出形成显式坏样本，worker报告并停止该桶，不能静默少算仍显示健康。
	log.MetricsAttempts = `{"incomplete":true}`
	if c.overflow {
		return
	}
	attempts := append([]MetricsAttempt(nil), c.attempts...)
	if log.Type == LogTypeConsume {
		a := &attempts[len(attempts)-1]
		a.Prompt, a.Completion, a.UsageKnown = int64(log.PromptTokens), int64(log.CompletionTokens), true
		if log.IsStream && log.FirstWordLatency > a.Offset {
			ttft := log.FirstWordLatency - a.Offset
			if !math.IsNaN(ttft) && ttft <= a.Duration {
				a.TTFT = ttft
			}
		}
	}
	payload, err := json.Marshal(attempts)
	if err != nil || len(payload) > 32768 {
		return
	}
	log.MetricsAttempts = string(payload)
}

func (c *MetricsCapture) finish() {
	c.Lock()
	c.finished = true
	pending := c.pending
	c.pending = nil
	for _, p := range pending {
		c.enrich(p.log)
	}
	c.Unlock()
	for _, p := range pending {
		p.write(p.log)
	}
}
