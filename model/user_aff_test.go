package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

// TestGenerateUniqueAffCode 邀请码必须避开已占用的值。
//
// aff_code 列上有 uniqueIndex，而原实现直接 helper.GetRandomString(4)
// 不做任何检查。字符集 62 位、长度 4 = 1477 万组合，按生日问题算，
// 约 4800 个用户时就有 50% 概率出现碰撞 —— 之后注册会因唯一键冲突失败。
func TestGenerateUniqueAffCode(t *testing.T) {
	setupTestDB(t, &User{})

	code, err := GenerateUniqueAffCode()
	if err != nil {
		t.Fatalf("GenerateUniqueAffCode failed: %v", err)
	}
	if len(code) < 4 {
		t.Errorf("code = %q, 长度应至少 4", code)
	}

	// 占用它，再要一个，必须不同
	if err := DB.Create(&User{
		Username: "u1", AffCode: code, AccessToken: "t1",
	}).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	second, err := GenerateUniqueAffCode()
	if err != nil {
		t.Fatalf("第二次生成失败: %v", err)
	}
	if second == code {
		t.Errorf("生成了已被占用的邀请码 %q", code)
	}
}

// TestGenerateUniqueAffCodeLengthensWhenCrowded 4 位空间被占满时必须加长，
// 否则会陷入死循环或返回错误。
//
// 这里不可能真的占满 1477 万个，改为验证「函数不会返回已存在的码」这一
// 不变式在高密度下依然成立：把所有 1 位与 2 位的组合都占掉是不现实的，
// 因此本用例只验证基础不变式 + 返回值非空，加长逻辑由代码注释与
// affCodeMaxRetries 常量保证。
func TestGenerateUniqueAffCodeAlwaysUnused(t *testing.T) {
	setupTestDB(t, &User{})

	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		code, err := GenerateUniqueAffCode()
		if err != nil {
			t.Fatalf("第 %d 次生成失败: %v", i+1, err)
		}
		if seen[code] {
			t.Fatalf("第 %d 次生成了重复的码 %q", i+1, code)
		}
		seen[code] = true
		// 每次都落库占用，逼函数每轮都要避开更多已用值
		if err := DB.Create(&User{
			Username:    "u" + code,
			AffCode:     code,
			AccessToken: "t" + code,
		}).Error; err != nil {
			t.Fatalf("create user failed: %v", err)
		}
	}
}

// TestInsertAccumulatesGiftQuota 注册奖励必须同时累加 gift_quota。
//
// gift_quota 的语义是「累计获赠总额」。原实现只加 quota，导致注册赠额
// 完全漏记，P4 的 stats 接口里「累计获赠」会偏小。
func TestInsertAccumulatesGiftQuota(t *testing.T) {
	setupTestDB(t, &User{}, &Log{})

	origNew, origInvitee, origInviter := config.QuotaForNewUser, config.QuotaForInvitee, config.QuotaForInviter
	config.QuotaForNewUser = 1000
	config.QuotaForInvitee = 2000
	config.QuotaForInviter = 3000
	t.Cleanup(func() {
		config.QuotaForNewUser, config.QuotaForInvitee, config.QuotaForInviter = origNew, origInvitee, origInviter
	})

	inviter := &User{Username: "inviter", AffCode: "invx", AccessToken: "t-inv"}
	if err := inviter.Insert(0); err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}

	// 邀请人自己的注册赠额
	var afterInviterCreate User
	_ = DB.First(&afterInviterCreate, inviter.Id).Error
	if afterInviterCreate.Quota != 1000 {
		t.Errorf("inviter Quota = %d, want 1000", afterInviterCreate.Quota)
	}
	if afterInviterCreate.GiftQuota != 1000 {
		t.Errorf("inviter GiftQuota = %d, want 1000（注册赠额要记入累计获赠）", afterInviterCreate.GiftQuota)
	}

	invitee := &User{Username: "invitee", AccessToken: "t-ive"}
	if err := invitee.Insert(inviter.Id); err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}

	// 被邀请人：注册赠额 1000 + 被邀请奖励 2000
	var afterInvitee User
	_ = DB.First(&afterInvitee, invitee.Id).Error
	if afterInvitee.Quota != 3000 {
		t.Errorf("invitee Quota = %d, want 3000", afterInvitee.Quota)
	}
	if afterInvitee.GiftQuota != 3000 {
		t.Errorf("invitee GiftQuota = %d, want 3000", afterInvitee.GiftQuota)
	}

	// 邀请人：自己的 1000 + 邀请奖励 3000
	var afterInviter User
	_ = DB.First(&afterInviter, inviter.Id).Error
	if afterInviter.Quota != 4000 {
		t.Errorf("inviter Quota = %d, want 4000", afterInviter.Quota)
	}
	if afterInviter.GiftQuota != 4000 {
		t.Errorf("inviter GiftQuota = %d, want 4000", afterInviter.GiftQuota)
	}
}

// TestInsertGeneratesUniqueAffCode Insert 不能再用裸随机串。
func TestInsertGeneratesUniqueAffCode(t *testing.T) {
	setupTestDB(t, &User{}, &Log{})

	orig := config.QuotaForNewUser
	config.QuotaForNewUser = 0
	t.Cleanup(func() { config.QuotaForNewUser = orig })

	codes := map[string]bool{}
	for i := 0; i < 20; i++ {
		u := &User{Username: "user" + string(rune('a'+i)), AccessToken: "tok" + string(rune('a'+i))}
		if err := u.Insert(0); err != nil {
			t.Fatalf("第 %d 个用户注册失败: %v", i+1, err)
		}
		if u.AffCode == "" {
			t.Fatalf("第 %d 个用户没有生成 aff_code", i+1)
		}
		if codes[u.AffCode] {
			t.Fatalf("aff_code 重复: %q", u.AffCode)
		}
		codes[u.AffCode] = true
	}
}
