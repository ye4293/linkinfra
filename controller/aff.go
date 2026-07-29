package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// affCodeSessionKey session 里存放邀请码的键。
const affCodeSessionKey = "aff_code"

// readAffCode 按「查询参数优先、session 兜底」取邀请码。
//
// 两个通道各自覆盖一条注册流程：
//   - 查询参数：POST /api/{provider}/login 这条前端直传流没有调用过
//     /api/oauth/state，session 里不会有邀请码
//   - session：GET /api/oauth/{provider}/callback 这条标准重定向流的回调
//     URL 由 OAuth 提供商拼装，前端无法附加参数；走 session 还能让邀请码
//     不出现在 URL 上（不进网关/CDN 访问日志）
//
// getSession 参数化是为了让这层取值逻辑能脱离真实 session store 测试。
// 签名与 gin-contrib/sessions 的 Session.Get 一致（interface{} 入参），
// 这样可以直接把 session.Get 传进来。
func readAffCode(c *gin.Context, getSession func(key interface{}) interface{}) string {
	if code := c.Query("aff_code"); code != "" {
		return code
	}
	if getSession == nil {
		return ""
	}
	if v, ok := getSession(affCodeSessionKey).(string); ok {
		return v
	}
	return ""
}

// resolveInviterId 解析出邀请人的用户 id；无邀请码或邀请码无效时返回 0。
//
// 邀请码无效不阻塞注册 —— 与密码注册流程（controller/user.go 里
// GetUserIdByAffCode 的 error 被忽略）保持一致。
func resolveInviterId(c *gin.Context) int {
	session := sessions.Default(c)
	code := readAffCode(c, session.Get)
	if code == "" {
		return 0
	}
	inviterId, err := model.GetUserIdByAffCode(code)
	if err != nil {
		return 0
	}
	return inviterId
}

// clearAffCodeSession 注册完成后清掉 session 里的邀请码，
// 避免同一浏览器后续的 OAuth 操作误用一个陈旧的邀请码。
func clearAffCodeSession(c *gin.Context) {
	session := sessions.Default(c)
	if session.Get(affCodeSessionKey) == nil {
		return
	}
	session.Delete(affCodeSessionKey)
	if err := session.Save(); err != nil {
		logger.SysError("failed to clear aff_code from session: " + err.Error())
	}
}

// maskUsername 对用户名脱敏：保留首尾字符，中间以 * 替代。
// 长度 ≤ 2 时全部替代。
//
// 邀请人能看到自己邀请了谁的返现明细，但不该拿到对方的完整账号 ——
// 那是可以用来撞库或社工的信息。
//
// 按 rune 而非 byte 处理：中文用户名按 byte 切会切出乱码。
func maskUsername(name string) string {
	runes := []rune(name)
	switch len(runes) {
	case 0:
		return ""
	case 1, 2:
		return strings.Repeat("*", len(runes))
	default:
		return string(runes[0]) + strings.Repeat("*", len(runes)-2) + string(runes[len(runes)-1])
	}
}

const (
	// affDefaultPageSize 未指定或非法时的每页条数。
	affDefaultPageSize = 10
	// affMaxPageSize 每页条数上限。
	//
	// 这几个接口是登录用户可直接调用的、参数完全由客户端控制。没有上限时
	// 一个 ?pagesize=10000000 就能让数据库尝试返回千万行 —— 内存打爆、
	// 慢查询拖垮 DB，是真实的 DoS 向量。
	//
	// 仓库里既有的分页接口（如 controller/topup.go）同样没有上限，那是
	// 既有问题；新接口不该照抄。
	affMaxPageSize = 100
)

// affParsePaging 解析分页参数。仓库约定：query 参数名是 page 与 pagesize
// （全小写），page < 1 归一为 1，pagesize 非法时取默认值、超限时钳到上限。
func affParsePaging(c *gin.Context) (page, pageSize int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(c.Query("pagesize"))
	if pageSize <= 0 {
		pageSize = affDefaultPageSize
	}
	if pageSize > affMaxPageSize {
		pageSize = affMaxPageSize
	}
	return page, pageSize
}

// affFail 按仓库约定返回失败：HTTP 200 + success:false。
func affFail(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, gin.H{"success": false, "message": msg})
}

// GetAffStats GET /api/user/aff/stats —— 当前用户的邀请汇总。
func GetAffStats(c *gin.Context) {
	stats, err := model.GetAffStats(c.GetInt("id"))
	if err != nil {
		affFail(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    stats,
	})
}

// GetAffCommissionRecords GET /api/user/aff/records —— 当前用户的返现明细。
// 被邀请人用户名脱敏后返回。
func GetAffCommissionRecords(c *gin.Context) {
	page, pageSize := affParsePaging(c)
	records, total, err := model.GetAffCommissionRecords(c.GetInt("id"), page, pageSize)
	if err != nil {
		affFail(c, err.Error())
		return
	}

	// 不直接把 model 结构体丢给前端：里面有 inviter_username、source_no
	// 等无需暴露的内部字段，而 invitee_username 必须脱敏
	type item struct {
		CreatedAt       int64   `json:"created_at"`
		InviteeUsername string  `json:"invitee_username"`
		SourceType      string  `json:"source_type"`
		TopupAmount     float64 `json:"topup_amount"`
		Rate            float64 `json:"rate"`
		CommissionQuota int64   `json:"commission_quota"`
		Status          int     `json:"status"`
		ReversedQuota   int64   `json:"reversed_quota"`
	}
	list := make([]item, 0, len(records))
	for _, r := range records {
		list = append(list, item{
			CreatedAt:       r.CreatedAt,
			InviteeUsername: maskUsername(r.InviteeUsername),
			SourceType:      r.SourceType,
			TopupAmount:     r.TopupAmount,
			Rate:            r.Rate,
			CommissionQuota: r.CommissionQuota,
			Status:          r.Status,
			ReversedQuota:   r.ReversedQuota,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        list,
			"currentPage": page,
			"pageSize":    pageSize,
			"total":       total,
		},
	})
}

// GetInvitees GET /api/user/invitees —— 当前用户邀请的人，用户名脱敏。
func GetInvitees(c *gin.Context) {
	page, pageSize := affParsePaging(c)
	invitees, total, err := model.GetInvitees(c.GetInt("id"), page, pageSize)
	if err != nil {
		affFail(c, err.Error())
		return
	}
	for i := range invitees {
		invitees[i].Username = maskUsername(invitees[i].Username)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        invitees,
			"currentPage": page,
			"pageSize":    pageSize,
			"total":       total,
		},
	})
}

// GetAffReport GET /api/aff/report —— 管理员侧全局返现报表。
//
// 管理员本就能查看用户完整信息，这里不脱敏。
func GetAffReport(c *gin.Context) {
	topN, _ := strconv.Atoi(c.Query("top"))
	if topN <= 0 || topN > 100 {
		topN = 10
	}
	report, err := model.GetAffReport(topN)
	if err != nil {
		affFail(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    report,
	})
}
