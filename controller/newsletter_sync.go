package controller

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/message"
	"github.com/songquanpeng/one-api/model"
)

var newsletterWake = make(chan struct{}, 1)
var newsletterSetupMu sync.Mutex

func wakeNewsletterSync() {
	select {
	case newsletterWake <- struct{}{}:
	default:
	}
}

// Only the master runs the queue. DB leases also protect rows from overlapping masters.
func StartNewsletterSync(ctx context.Context) {
	if !config.IsMasterNode {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		syncNewsletterBatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-newsletterWake:
		}
	}
}

func syncNewsletterBatch(ctx context.Context) {
	client := message.NewNewsletterClient()
	if !client.Configured() {
		return
	}
	rows, err := model.PendingNewsletterSubscribers(client.SegmentID, 10)
	if err != nil {
		logger.SysError("newsletter queue read failed")
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		claimed, err := model.ClaimNewsletterSubscriber(row.ID, client.SegmentID)
		if err != nil || !claimed {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		contact, syncErr := client.SyncContact(requestCtx, row.Email)
		cancel()
		if err := model.CompleteNewsletterSync(row, client.SegmentID, contact.ID, contact.Unsubscribed, syncErr); err != nil {
			logger.SysError("newsletter queue update failed")
		}
	}
}

func GetNewsletterStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	client := message.NewNewsletterClient()
	stats, err := model.GetNewsletterSyncStats(client.SegmentID)
	if err != nil {
		c.JSON(503, gin.H{"success": false, "message": "subscription_unavailable"})
		return
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{"configured": client.Configured(), "segment_id": client.SegmentID, "stats": stats}})
}

func RetryNewsletterSync(c *gin.Context) {
	client := message.NewNewsletterClient()
	if !client.Configured() {
		c.JSON(400, gin.H{"success": false, "message": "resend_not_configured"})
		return
	}
	if err := model.RetryNewsletterSync(client.SegmentID); err != nil {
		c.JSON(503, gin.H{"success": false, "message": "subscription_unavailable"})
		return
	}
	wakeNewsletterSync()
	c.JSON(http.StatusAccepted, gin.H{"success": true, "message": "sync_queued"})
}

// Root-only: validate or create a dedicated newsletter segment, then persist it.
func ConfigureNewsletter(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var request struct {
		SegmentID string `json:"segment_id"`
		Create    bool   `json:"create"`
	}
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid_request"})
		return
	}
	newsletterSetupMu.Lock()
	defer newsletterSetupMu.Unlock()
	client := message.NewNewsletterClient()
	id := strings.TrimSpace(request.SegmentID)
	var err error
	if request.Create {
		id = client.SegmentID
		if id == "" {
			config.OptionMapRWMutex.RLock()
			name := config.SystemName + " Newsletter"
			config.OptionMapRWMutex.RUnlock()
			id, err = client.CreateSegment(c.Request.Context(), name)
		}
	} else {
		if _, parseErr := uuid.Parse(id); parseErr != nil {
			c.JSON(400, gin.H{"success": false, "message": "invalid_segment"})
			return
		}
		err = client.CheckSegment(c.Request.Context(), id)
	}
	if err != nil {
		c.JSON(200, gin.H{"success": false, "message": err.Error()})
		return
	}
	if err := model.UpdateOption(message.NewsletterSegmentOption, id); err != nil {
		c.JSON(503, gin.H{"success": false, "message": "configuration_save_failed"})
		return
	}
	wakeNewsletterSync()
	c.JSON(200, gin.H{"success": true, "data": gin.H{"segment_id": id}})
}
