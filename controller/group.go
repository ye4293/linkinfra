package controller

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

func GetGroups(c *gin.Context) {
	groupNames := make([]string, 0)
	for groupName := range common.GroupRatio {
		groupNames = append(groupNames, groupName)
	}

	// 只排序内存里已有的 key，不把未生效的配置加入返回集合。
	configs, _ := model.GetAllGroupConfigs()
	orders := make(map[string]int, len(configs))
	for _, cfg := range configs {
		orders[cfg.GroupKey] = cfg.SortOrder
	}
	sort.Slice(groupNames, func(i, j int) bool {
		a, aKnown := orders[groupNames[i]]
		b, bKnown := orders[groupNames[j]]
		if aKnown != bKnown {
			return aKnown
		}
		if aKnown && a != b {
			return a < b
		}
		return groupNames[i] < groupNames[j]
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    groupNames,
	})
}
