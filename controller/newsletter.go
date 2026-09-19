package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
)

func SubscribeNewsletter(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var request struct {
		Email    string `json:"email"`
		Language string `json:"language"`
		Consent  bool   `json:"consent"`
	}
	if c.ShouldBindJSON(&request) != nil || !request.Consent {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid_request"})
		return
	}
	err := model.SubscribeNewsletter(request.Email, request.Language)
	if errors.Is(err, model.ErrInvalidNewsletterEmail) {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid_email"})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "subscription_unavailable"})
		return
	}
	// Do not disclose whether this address was already subscribed.
	wakeNewsletterSync()
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "subscribed"})
}

func GetNewsletterSubscribers(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 || page > 1000000 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid_page"})
		return
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if err != nil || pageSize < 1 || pageSize > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid_page_size"})
		return
	}
	subscribers, total, err := model.ListNewsletterSubscribers(page, pageSize)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "subscription_unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"items": subscribers, "total": total, "page": page, "page_size": pageSize,
	}})
}
