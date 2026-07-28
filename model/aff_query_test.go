package model

import "testing"

// affQueryFixture 造 1 个邀请人 + 3 个被邀请人 + 若干返现记录。
func affQueryFixture(t *testing.T) (inviterId int) {
	t.Helper()

	inviter := &User{Username: "inviter", Group: "Lv4", AffCode: "inv", AccessToken: "t-inv"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}

	// 3 个被邀请人，其中 2 个有充值记录
	names := []string{"alice", "bob", "carol"}
	ids := make([]int, len(names))
	for i, n := range names {
		u := &User{
			Username: n, Group: "Lv1", AffCode: n, AccessToken: "t-" + n,
			InviterId: inviter.Id,
		}
		if err := DB.Create(u).Error; err != nil {
			t.Fatalf("create %s failed: %v", n, err)
		}
		ids[i] = u.Id
	}
	// alice 与 bob 有充值
	_ = DB.Model(&User{}).Where("id = ?", ids[0]).Update("topup_quota", 50000000).Error
	_ = DB.Model(&User{}).Where("id = ?", ids[1]).Update("topup_quota", 10000000).Error

	// 别人的被邀请人，不能串进来
	other := &User{Username: "other", AffCode: "oth", AccessToken: "t-oth", InviterId: 99999}
	if err := DB.Create(other).Error; err != nil {
		t.Fatalf("create other failed: %v", err)
	}

	recs := []AffCommissionRecord{
		{InviterId: inviter.Id, InviteeId: ids[0], InviteeUsername: "alice",
			InviterUsername: "inviter",
			SourceType:      SourceTypeStripeCheckout, SourceNo: "r1",
			TopupAmount: 100, Rate: 0.08, InviterGroup: "Lv4",
			CommissionQuota: 4000000, Status: AffCommissionStatusGranted, CreatedAt: 300},
		{InviterId: inviter.Id, InviteeId: ids[1], InviteeUsername: "bob",
			InviterUsername: "inviter",
			SourceType:      SourceTypeStripeCharge, SourceNo: "r2",
			TopupAmount: 20, Rate: 0.08, InviterGroup: "Lv4",
			CommissionQuota: 800000, Status: AffCommissionStatusGranted, CreatedAt: 200},
		{InviterId: inviter.Id, InviteeId: ids[0], InviteeUsername: "alice",
			InviterUsername: "inviter",
			SourceType:      SourceTypeStripeCheckout, SourceNo: "r3",
			TopupAmount: 50, Rate: 0.08, InviterGroup: "Lv4",
			CommissionQuota: 2000000, Status: AffCommissionStatusReversed,
			ReversedQuota: 2000000, CreatedAt: 100},
		// 别人的返现记录
		{InviterId: 99999, InviteeId: other.Id, InviteeUsername: "other",
			InviterUsername: "someone", SourceNo: "r4",
			CommissionQuota: 999, Status: AffCommissionStatusGranted, CreatedAt: 400},
	}
	for i := range recs {
		if err := DB.Create(&recs[i]).Error; err != nil {
			t.Fatalf("create record failed: %v", err)
		}
	}
	return inviter.Id
}

func TestGetAffCommissionRecords(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	records, total, err := GetAffCommissionRecords(inviterId, 1, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3（含已冲正的，明细要能看到全部）", total)
	}
	if len(records) != 3 {
		t.Fatalf("len = %d, want 3", len(records))
	}
	// 按 created_at 倒序：r1(300) > r2(200) > r3(100)
	wantOrder := []string{"r1", "r2", "r3"}
	for i, want := range wantOrder {
		if records[i].SourceNo != want {
			t.Errorf("位置 %d = %q, want %q", i, records[i].SourceNo, want)
		}
	}
	// 不能串到别人的记录
	for _, r := range records {
		if r.InviterId != inviterId {
			t.Errorf("串入了他人记录: %+v", r)
		}
	}
}

func TestGetAffCommissionRecordsPaging(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	page1, total, err := GetAffCommissionRecords(inviterId, 1, 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3（total 是全量，不受分页影响）", total)
	}
	if len(page1) != 2 {
		t.Errorf("第一页 len = %d, want 2", len(page1))
	}

	page2, _, err := GetAffCommissionRecords(inviterId, 2, 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(page2) != 1 {
		t.Errorf("第二页 len = %d, want 1", len(page2))
	}
	if page2[0].SourceNo == page1[0].SourceNo {
		t.Error("第二页与第一页内容重复，offset 计算有误")
	}
}

func TestGetInvitees(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	invitees, total, err := GetInvitees(inviterId, 1, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(invitees) != 3 {
		t.Fatalf("len = %d, want 3", len(invitees))
	}

	paid := 0
	for _, iv := range invitees {
		if iv.HasPaid {
			paid++
		}
	}
	if paid != 2 {
		t.Errorf("已充值人数 = %d, want 2", paid)
	}
}

// TestGetInviteesEmptyReturnsSlice 空结果必须是 []，不能是 nil ——
// 前端拿到 null 会崩。
func TestGetInviteesEmptyReturnsSlice(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})

	invitees, total, err := GetInvitees(12345, 1, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if invitees == nil {
		t.Error("返回了 nil，应为空切片")
	}
}

func TestGetAffStats(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{}, &GroupConfig{})
	inviterId := affQueryFixture(t)

	if err := DB.Create(&GroupConfig{
		GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, CommissionRate: 0.08,
	}).Error; err != nil {
		t.Fatalf("seed group failed: %v", err)
	}

	stats, err := GetAffStats(inviterId)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if stats.InviteeCount != 3 {
		t.Errorf("InviteeCount = %d, want 3", stats.InviteeCount)
	}
	if stats.PaidInviteeCount != 2 {
		t.Errorf("PaidInviteeCount = %d, want 2", stats.PaidInviteeCount)
	}
	// 已冲正的 2000000 不计入累计收益
	if stats.TotalCommission != 4800000 {
		t.Errorf("TotalCommission = %d, want 4800000", stats.TotalCommission)
	}
	if stats.CommissionCount != 2 {
		t.Errorf("CommissionCount = %d, want 2", stats.CommissionCount)
	}
	if stats.AffCode != "inv" {
		t.Errorf("AffCode = %q, want inv", stats.AffCode)
	}
	if stats.CommissionRate != 0.08 {
		t.Errorf("CommissionRate = %v, want 0.08", stats.CommissionRate)
	}
}

// TestGetAffStatsMissingGroupConfig 分组配置缺失时比例按 0，不报错。
func TestGetAffStatsMissingGroupConfig(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{}, &GroupConfig{})
	inviterId := affQueryFixture(t)

	stats, err := GetAffStats(inviterId)
	if err != nil {
		t.Fatalf("分组配置缺失不应报错: %v", err)
	}
	if stats.CommissionRate != 0 {
		t.Errorf("CommissionRate = %v, want 0", stats.CommissionRate)
	}
}

func TestGetAffReport(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	affQueryFixture(t)

	report, err := GetAffReport(10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	// 全局：inviter 的 4000000+800000，加上 99999 的 999
	if report.TotalCommission != 4800999 {
		t.Errorf("TotalCommission = %d, want 4800999", report.TotalCommission)
	}
	if report.TotalReversed != 2000000 {
		t.Errorf("TotalReversed = %d, want 2000000", report.TotalReversed)
	}
	// 冲正扣满了，无损失
	if report.ReversedLoss != 0 {
		t.Errorf("ReversedLoss = %d, want 0", report.ReversedLoss)
	}
	if len(report.TopInviters) == 0 {
		t.Fatal("TopInviters 为空")
	}
	// 按返现额降序，第一名应是发放了 4800000 的那个
	if report.TopInviters[0].TotalCommission != 4800000 {
		t.Errorf("第一名返现额 = %d, want 4800000", report.TopInviters[0].TotalCommission)
	}
	if report.TopInviters[0].RecordCount != 2 {
		t.Errorf("第一名笔数 = %d, want 2", report.TopInviters[0].RecordCount)
	}
}

// TestGetAffReportReversedLoss 冲正时余额不足没扣满的差额要能查出来。
func TestGetAffReportReversedLoss(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})

	// 应扣 4000000，实际只扣回 1000000，损失 3000000
	if err := DB.Create(&AffCommissionRecord{
		InviterId: 1, InviteeId: 2, SourceNo: "loss",
		CommissionQuota: 4000000, ReversedQuota: 1000000,
		Status: AffCommissionStatusReversed, CreatedAt: 1,
	}).Error; err != nil {
		t.Fatalf("create failed: %v", err)
	}

	report, err := GetAffReport(10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if report.ReversedLoss != 3000000 {
		t.Errorf("ReversedLoss = %d, want 3000000", report.ReversedLoss)
	}
}
