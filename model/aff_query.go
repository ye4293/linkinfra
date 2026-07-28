package model

// AffStats 邀请汇总统计，供 GET /api/user/aff/stats 使用。
type AffStats struct {
	AffCode          string  `json:"aff_code"`
	InviteeCount     int64   `json:"invitee_count"`      // 已邀请人数
	PaidInviteeCount int64   `json:"paid_invitee_count"` // 其中已充值的人数
	TotalCommission  int64   `json:"total_commission"`   // 累计返现（不含已冲正）
	CommissionCount  int64   `json:"commission_count"`   // 有效返现笔数
	CurrentGroup     string  `json:"current_group"`      // 当前等级
	CommissionRate   float64 `json:"commission_rate"`    // 当前等级的返现比例
}

// InviteeItem 被邀请人列表项。
//
// 不含注册时间：users 表没有创建时间列，加列属于 schema 变更，不在本期范围。
type InviteeItem struct {
	UserId     int    `json:"user_id"`
	Username   string `json:"username"` // controller 层脱敏后返回
	Group      string `json:"group"`
	HasPaid    bool   `json:"has_paid"`
	TopupQuota int64  `json:"topup_quota"`
}

// TopInviter 管理员报表里的推广人排行项。
type TopInviter struct {
	InviterId       int    `json:"inviter_id"`
	InviterUsername string `json:"inviter_username"`
	TotalCommission int64  `json:"total_commission"`
	RecordCount     int64  `json:"record_count"`
}

// AffReport 管理员侧全局返现报表。
type AffReport struct {
	TotalCommission int64        `json:"total_commission"` // 全局累计发放（不含已冲正）
	TotalReversed   int64        `json:"total_reversed"`   // 全局累计冲正额
	ReversedLoss    int64        `json:"reversed_loss"`    // 冲正时因余额不足没扣回的差额
	TopInviters     []TopInviter `json:"top_inviters"`
}

// GetAffStats 汇总某个邀请人的邀请与返现情况。
func GetAffStats(inviterId int) (*AffStats, error) {
	stats := &AffStats{}
	if inviterId <= 0 {
		return stats, nil
	}

	var user User
	if err := DB.Where("id = ?", inviterId).First(&user).Error; err != nil {
		return nil, err
	}
	stats.AffCode = user.AffCode
	stats.CurrentGroup = user.Group

	// 分组配置可能不存在（手工改过 DB 留下的野分组），
	// 此时比例按 0 处理而非报错 —— 汇总接口不该因为配置问题整个挂掉
	if gc, err := GetGroupConfigByKey(user.Group); err == nil {
		stats.CommissionRate = gc.CommissionRate
	}

	if err := DB.Model(&User{}).Where("inviter_id = ?", inviterId).
		Count(&stats.InviteeCount).Error; err != nil {
		return nil, err
	}
	if err := DB.Model(&User{}).Where("inviter_id = ? AND topup_quota > 0", inviterId).
		Count(&stats.PaidInviteeCount).Error; err != nil {
		return nil, err
	}

	total, count, err := GetAffCommissionSummary(inviterId)
	if err != nil {
		return nil, err
	}
	stats.TotalCommission = total
	stats.CommissionCount = count

	return stats, nil
}

// GetAffCommissionRecords 分页查询某个邀请人的返现明细，按时间倒序。
// 已冲正的记录也会返回 —— 用户需要看到「这笔为什么被扣回」。
func GetAffCommissionRecords(inviterId, page, pageSize int) ([]AffCommissionRecord, int64, error) {
	records := []AffCommissionRecord{}
	var total int64
	if inviterId <= 0 {
		return records, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	if err := DB.Model(&AffCommissionRecord{}).Where("inviter_id = ?", inviterId).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := DB.Where("inviter_id = ?", inviterId).
		Order("created_at desc, id desc").
		Limit(pageSize).Offset((page - 1) * pageSize).
		Find(&records).Error
	if err != nil {
		return nil, total, err
	}
	return records, total, nil
}

// GetInvitees 分页查询某个邀请人邀请的用户。
func GetInvitees(inviterId, page, pageSize int) ([]InviteeItem, int64, error) {
	// 用 []T{} 而非 nil 初始化：JSON 序列化后前端拿到 [] 而不是 null
	items := []InviteeItem{}
	var total int64
	if inviterId <= 0 {
		return items, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	if err := DB.Model(&User{}).Where("inviter_id = ?", inviterId).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var users []User
	err := DB.Where("inviter_id = ?", inviterId).
		Order("id desc").
		Limit(pageSize).Offset((page - 1) * pageSize).
		Find(&users).Error
	if err != nil {
		return nil, total, err
	}

	for i := range users {
		items = append(items, InviteeItem{
			UserId:     users[i].Id,
			Username:   users[i].Username,
			Group:      users[i].Group,
			HasPaid:    users[i].TopupQuota > 0,
			TopupQuota: users[i].TopupQuota,
		})
	}
	return items, total, nil
}

// GetAffReport 生成管理员侧的全局返现报表。
func GetAffReport(topN int) (*AffReport, error) {
	if topN <= 0 {
		topN = 10
	}
	report := &AffReport{TopInviters: []TopInviter{}}

	if err := DB.Model(&AffCommissionRecord{}).
		Where("status = ?", AffCommissionStatusGranted).
		Select("COALESCE(SUM(commission_quota), 0)").
		Scan(&report.TotalCommission).Error; err != nil {
		return nil, err
	}

	if err := DB.Model(&AffCommissionRecord{}).
		Where("status = ?", AffCommissionStatusReversed).
		Select("COALESCE(SUM(reversed_quota), 0)").
		Scan(&report.TotalReversed).Error; err != nil {
		return nil, err
	}

	// 冲正时因邀请人余额不足而没扣回的差额，是运营的真实损失
	if err := DB.Model(&AffCommissionRecord{}).
		Where("status = ?", AffCommissionStatusReversed).
		Select("COALESCE(SUM(commission_quota - reversed_quota), 0)").
		Scan(&report.ReversedLoss).Error; err != nil {
		return nil, err
	}

	rows := []struct {
		InviterId       int
		InviterUsername string
		TotalCommission int64
		RecordCount     int64
	}{}
	err := DB.Model(&AffCommissionRecord{}).
		Select("inviter_id, MAX(inviter_username) as inviter_username, " +
			"SUM(commission_quota) as total_commission, COUNT(*) as record_count").
		Where("status = ?", AffCommissionStatusGranted).
		Group("inviter_id").
		Order("total_commission desc").
		Limit(topN).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		report.TopInviters = append(report.TopInviters, TopInviter{
			InviterId:       r.InviterId,
			InviterUsername: r.InviterUsername,
			TotalCommission: r.TotalCommission,
			RecordCount:     r.RecordCount,
		})
	}

	return report, nil
}
