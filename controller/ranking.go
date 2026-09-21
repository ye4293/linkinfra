package controller

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

func GetRankings(c *gin.Context) {
	if !config.LogConsumeEnabled {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"success": false, "status": "paused", "data": nil})
		return
	}
	if !config.RankingsEnabled {
		c.JSON(http.StatusOK, gin.H{"success": false, "status": "disabled", "data": nil})
		return
	}
	cached := model.GetCachedRanking()
	if cached == nil {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"success": false, "status": "preparing", "data": nil})
		return
	}
	c.Header("Cache-Control", "public, max-age=60, s-maxage=300")
	c.Header("ETag", cached.ETag)
	if c.GetHeader("If-None-Match") == cached.ETag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", cached.Payload)
}

func RebuildRankingDay(c *gin.Context) {
	var input struct {
		Date string `json:"date"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "date is required"})
		return
	}
	day, err := time.Parse("2006-01-02", input.Date)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "expected YYYY-MM-DD in UTC"})
		return
	}
	if err = model.RequestRankingRebuild(day.Unix()); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}
