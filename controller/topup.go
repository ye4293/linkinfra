package controller

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/model"
)

var orderLocks sync.Map
var createLock sync.Mutex

func LockOrder(tradeNo string) {
	lock, ok := orderLocks.Load(tradeNo)
	if !ok {
		createLock.Lock()
		defer createLock.Unlock()
		lock, ok = orderLocks.Load(tradeNo)
		if !ok {
			lock = new(sync.Mutex)
			orderLocks.Store(tradeNo, lock)
		}
	}
	lock.(*sync.Mutex).Lock()
}

func UnlockOrder(tradeNo string) {
	lock, ok := orderLocks.Load(tradeNo)
	if ok {
		lock.(*sync.Mutex).Unlock()
		orderLocks.Delete(tradeNo)
	}
}

func CompleteTopUp(c *gin.Context) {
	role := c.GetInt("role")
	if role < common.RoleAdminUser {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Only admins can manually complete orders."})
		return
	}

	var req struct {
		TradeNo string `json:"trade_no"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.TradeNo == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Order number is required."})
		return
	}

	LockOrder(req.TradeNo)
	defer UnlockOrder(req.TradeNo)

	opId := c.GetInt("id")
	meta := model.TopUpManualCompleteMeta{
		Source:         "manual_complete",
		OperatorUserId: opId,
		OperatorUsername: c.GetString("username"),
		CompletedAt:    helper.GetTimestamp(),
	}
	
	err := model.CompleteTopUpOrderManual(req.TradeNo, meta)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "Order completed."})
}

func GetUserTopUps(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.Query("pagesize"))
	if pageSize <= 0 {
		pageSize = 10
	}
	tradeNo := c.Query("trade_no")

	userId := c.GetInt("id")
	role := c.GetInt("role")
	queryUserId := userId
	if role >= common.RoleAdminUser {
		queryUserId = 0
	}

	topups, total, err := model.SearchTopUps(queryUserId, tradeNo, page, pageSize)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        topups,
			"currentPage": page,
			"pageSize":    pageSize,
			"total":       total,
		},
	})
}
