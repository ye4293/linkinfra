package controller

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
)

// adminInfoRegex 匹配 ezlinkai 分号格式的 adminInfo:[...] 段
// 渠道历史是 []int，不会出现嵌套 ']'
var adminInfoRegex = regexp.MustCompile(`adminInfo:\[[^\]]*\];?`)

// billingTagDetailRegex 匹配 billingDetails 里的 "billing_tag":"..." 项及相邻的一个逗号，
// 剥离后仍是合法 JSON。计费标签只含 a-z 0-9 . _ -，值里不会出现引号或转义。
var billingTagDetailRegex = regexp.MustCompile(`"billing_tag":"[^"]*",?|,"billing_tag":"[^"]*"`)

// stripAdminInfoFromLogs 从普通用户日志的 other 字段里剥离渠道链路信息
// （adminInfo / retryHistory，以及 billingDetails 里的 billing_tag）
// 用于 /api/log/self 等用户接口，避免向终端用户暴露后端渠道 ID、渠道名、重试明细和计费标签
//
// 兼容两种 other 格式：
//   - 分号格式: "adminInfo:[1,2];retryHistory:[{...}];usageDetails:{...};..."
//   - JSON 格式: {"admin_info":[...],"retry_history":[...],"usageDetails":{...},...}
//
// fast-path: 不含 "admin"、"retryHistory" 和 "billing_tag" 子串直接跳过
func stripAdminInfoFromLogs(logs []*model.Log) {
	for _, log := range logs {
		if log == nil || log.Other == "" {
			continue
		}
		// fast-path: 没有 admin、retryHistory 和 billing_tag → 跳过
		if !strings.Contains(log.Other, "admin") && !strings.Contains(log.Other, "retryHistory") && !strings.Contains(log.Other, "retry_history") && !strings.Contains(log.Other, "billing_tag") {
			continue
		}

		other := log.Other
		// JSON 格式：以 '{' 开头时尝试解析
		if other[0] == '{' {
			var obj map[string]any
			if err := json.Unmarshal([]byte(other), &obj); err == nil {
				delete(obj, "admin_info")
				delete(obj, "adminInfo")
				delete(obj, "retry_history")
				delete(obj, "retryHistory")
				// 计费标签：平铺在顶层、或嵌在计费明细里（两种 key 写法）都去掉
				delete(obj, "billing_tag")
				for _, key := range []string{"billingDetails", "billing_details"} {
					if details, ok := obj[key].(map[string]any); ok {
						delete(details, "billing_tag")
					}
				}
				if b, err := json.Marshal(obj); err == nil {
					log.Other = string(b)
				}
				continue
			}
			// JSON 解析失败，落回分号格式处理
		}

		// 分号格式
		cleaned := adminInfoRegex.ReplaceAllString(other, "")
		cleaned = stripRetryHistorySegment(cleaned)
		cleaned = stripBillingTagDetail(cleaned)
		// 避免出现 ";;" 残留
		cleaned = strings.ReplaceAll(cleaned, ";;", ";")
		log.Other = strings.Trim(cleaned, ";")
	}
}

// stripBillingTagDetail 去掉分号格式 other 里计费明细的 billing_tag。
// 只有命中标签折扣的日志才带这个字段，其余的不必过正则。替换只作用于 billingDetails 段及其之后：
// 它前面的段（例如模型重定向记录的原始模型名）里如果恰好出现同样的字样，整串替换会把真正的标签留下来。
func stripBillingTagDetail(s string) string {
	if !strings.Contains(s, "billing_tag") {
		return s
	}
	start := strings.LastIndex(s, "billingDetails:")
	if start < 0 {
		start = 0
	}
	return s[:start] + billingTagDetailRegex.ReplaceAllString(s[start:], "")
}

// stripRetryHistorySegment 从分号格式 other 中剥掉 retryHistory:[...] 段。
// retryHistory 是 JSON 对象数组（含嵌套 {} 和 ""），不能用简单 regex，需要手动方括号配对。
// 配对时尊重字符串字面量内的 [ ] 以及反斜杠转义。
func stripRetryHistorySegment(s string) string {
	const key = "retryHistory:["
	idx := strings.Index(s, key)
	if idx == -1 {
		return s
	}
	start := idx + len(key) - 1 // 指向开头的 '['
	depth := 0
	inStr := false
	escape := false
	end := -1
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if escape {
				escape = false
				continue
			}
			if ch == '\\' {
				escape = true
				continue
			}
			if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		// 解析失败（不该发生），保留原 string 避免破坏数据
		return s
	}
	// 把整个 "retryHistory:[...]" 段（含可能紧跟的 ';'）切掉
	stop := end + 1
	if stop < len(s) && s[stop] == ';' {
		stop++
	}
	return s[:idx] + s[stop:]
}

func GetAllLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	if page < 0 {
		page = 0
	}
	pagesize, _ := strconv.Atoi(c.Query("pagesize"))
	currentPage := page
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	username := c.Query("username")
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	xRequestId := c.Query("x_request_id")
	xResponseId := c.Query("x_response_id")
	channel, _ := strconv.Atoi(c.Query("channel"))
	logs, total, err := model.GetCurrentAllLogsAndCount(logType, startTimestamp, endTimestamp, modelName, username, tokenName, xRequestId, xResponseId, page, pagesize, channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        logs,
			"currentPage": currentPage,
			"pageSize":    pagesize,
			"total":       total,
		},
	})
	return
}

func GetUserLogs(c *gin.Context) {
	pageStr := c.Query("page")
	pageSizeStr := c.Query("pagesize")

	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		page = 1
	}

	pagesize, err := strconv.Atoi(pageSizeStr)
	if err != nil || pagesize <= 0 {
		pagesize = 10
	}
	currentPage := page

	userId := c.GetInt("id")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	xRequestId := c.Query("x_request_id")
	xResponseId := c.Query("x_response_id")
	logs, total, err := model.GetCurrentUserLogsAndCount(userId, logType, startTimestamp, endTimestamp, modelName, tokenName, xRequestId, xResponseId, page, pagesize)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	stripAdminInfoFromLogs(logs)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        logs,
			"currentPage": currentPage,
			"pageSize":    pagesize,
			"total":       total,
		},
	})
	return
}

func SearchAllLogs(c *gin.Context) {
	keyword := c.Query("keyword")
	logs, err := model.SearchAllLogs(keyword)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    logs,
	})
	return
}

func SearchUserLogs(c *gin.Context) {
	keyword := c.Query("keyword")
	userId := c.GetInt("id")
	logs, err := model.SearchUserLogs(userId, keyword)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	stripAdminInfoFromLogs(logs)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    logs,
	})
	return
}

func GetLogsStat(c *gin.Context) {
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	username := c.Query("username")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	quotaNum := model.SumUsedQuota(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel)
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, "")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota": quotaNum,
			//"token": tokenNum,
		},
	})
	return
}

func GetLogsSelfStat(c *gin.Context) {
	username := c.GetString("username")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	quotaNum := model.SumUsedQuota(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel)
	//tokenNum := model.SumUsedToken(logType, startTimestamp, endTimestamp, modelName, username, tokenName)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"quota": quotaNum,
			//"token": tokenNum,
		},
	})
	return
}

func DeleteHistoryLogs(c *gin.Context) {
	targetTimestamp, _ := strconv.ParseInt(c.Query("target_timestamp"), 10, 64)
	if targetTimestamp == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "target timestamp is required",
		})
		return
	}
	count, err := model.DeleteOldLog(targetTimestamp)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    count,
	})
	return
}

// parseTimeBucket 将前端传入的时间粒度字符串转换为秒数
func parseTimeBucket(bucket string) int64 {
	switch bucket {
	case "15m":
		return 900
	case "1h":
		return 3600
	default: // "5m" 或其他
		return 300
	}
}

func GetLogsPerformanceStat(c *gin.Context) {
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	username := c.Query("username")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	bucketSeconds := parseTimeBucket(c.Query("time_bucket"))

	if startTimestamp == 0 || endTimestamp == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "start_timestamp and end_timestamp are required",
		})
		return
	}

	result, err := model.GetPerformanceStat(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel, bucketSeconds)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    result,
	})
}

func GetLogsSelfPerformanceStat(c *gin.Context) {
	userId := c.GetInt("id")
	logType, _ := strconv.Atoi(c.Query("type"))
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	channel, _ := strconv.Atoi(c.Query("channel"))
	bucketSeconds := parseTimeBucket(c.Query("time_bucket"))

	if startTimestamp == 0 || endTimestamp == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "start_timestamp and end_timestamp are required",
		})
		return
	}

	result, err := model.GetUserPerformanceStat(userId, logType, startTimestamp, endTimestamp, modelName, tokenName, channel, bucketSeconds)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    result,
	})
}
