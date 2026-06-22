package controller

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

// GetAffinityConfig GET /api/affinity/config
func GetAffinityConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    common.ChannelAffinityConfig,
	})
}

// UpdateAffinityConfig PUT /api/affinity/config
func UpdateAffinityConfig(c *gin.Context) {
	var cfg common.ChannelAffinitySetting
	if err := json.NewDecoder(c.Request.Body).Decode(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Failed to parse request: " + err.Error()})
		return
	}
	jsonStr := common.AffinityConfigToJSON(cfg)
	if err := model.UpdateOption("ChannelAffinityConfig", jsonStr); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Save failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Saved."})
}

// GetAffinityCacheStats GET /api/affinity/cache — 返回缓存条目数
func GetAffinityCacheStats(c *gin.Context) {
	if !common.RedisEnabled || common.RDB == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"count": 0, "redis_enabled": false}})
		return
	}
	var count int64
	var cursor uint64
	const prefix = "channel_affinity:v1:*"
	for {
		keys, nextCursor, err := common.RDB.Scan(context.Background(), cursor, prefix, 100).Result()
		if err != nil {
			break
		}
		count += int64(len(keys))
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"count": count, "redis_enabled": true},
	})
}

// ClearAffinityCache DELETE /api/affinity/cache — 清空所有亲和缓存
func ClearAffinityCache(c *gin.Context) {
	if !common.RedisEnabled || common.RDB == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Redis is not enabled; nothing to clear."})
		return
	}
	var cursor uint64
	const prefix = "channel_affinity:v1:*"
	var deleted int64
	for {
		keys, nextCursor, err := common.RDB.Scan(context.Background(), cursor, prefix, 100).Result()
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "Scan failed: " + err.Error()})
			return
		}
		if len(keys) > 0 {
			if err := common.RDB.Del(context.Background(), keys...).Err(); err != nil {
				c.JSON(http.StatusOK, gin.H{"success": false, "message": "Delete failed: " + err.Error()})
				return
			}
			deleted += int64(len(keys))
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Affinity cache cleared.",
		"data":    gin.H{"deleted": deleted},
	})
}
