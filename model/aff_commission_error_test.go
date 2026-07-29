package model

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// TestGrantCommissionDistinguishesRealDBError GrantCommission 必须区分
// 「分组配置不存在」与「真实 DB 故障」。
//
// 设计意图是：分组配置缺失时降级为不返现，不阻塞充值入账。但原实现把
// GetGroupConfigByKeyTx 返回的**所有** error 都当成「配置缺失」吞掉，
// 包括表不存在、连接断开这类真实故障。
//
// 在 PostgreSQL 上这尤其危险：事务内任何一条语句失败后，整个事务进入
// aborted 状态，后续语句全部报 "current transaction is aborted"。
// 吞掉真实错误后调用方会继续在这个已废的事务里做事，最终以一个与根因
// 无关的错误失败，排查时完全找不到方向。
//
// 正确行为：ErrRecordNotFound → 降级不返现；其余 error → 返回，回滚事务。
func TestGrantCommissionDistinguishesRealDBError(t *testing.T) {
	// 刻意不迁移 GroupConfig 表，模拟「表不存在」这类真实 DB 故障
	setupTestDB(t, &User{}, &AffCommissionRecord{}, &Log{})

	inviter := &User{Username: "inviter", Group: "Lv4", AffCode: "inv", AccessToken: "t-inv"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}
	invitee := &User{Username: "invitee", Group: "Lv1", AffCode: "ive", AccessToken: "t-ive", InviterId: inviter.Id}
	if err := DB.Create(invitee).Error; err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}

	var gotErr error
	_ = DB.Transaction(func(tx *gorm.DB) error {
		_, _, gotErr = GrantCommission(tx, invitee.Id, 100, 50000000,
			SourceTypeStripeCheckout, "trade-dberr")
		// 不管 GrantCommission 返回什么都提交，这里只关心它的返回值
		return nil
	})

	if gotErr == nil {
		t.Error("group_configs 表不存在（真实 DB 故障）时应返回错误以回滚事务，" +
			"而不是当成「配置缺失」静默降级")
	}
	if errors.Is(gotErr, gorm.ErrRecordNotFound) {
		t.Errorf("不该把真实 DB 故障归类为 ErrRecordNotFound: %v", gotErr)
	}
}

// TestGrantCommissionMissingGroupConfigStillDegrades 配置真的不存在时
// 仍要降级为不返现（不能因为上面的修复把这条也变成报错）。
func TestGrantCommissionMissingGroupConfigStillDegrades(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})

	// 表存在但没有 Lv4 这一行
	inviter := &User{Username: "inviter", Group: "Lv4", AffCode: "inv", AccessToken: "t-inv"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}
	invitee := &User{Username: "invitee", Group: "Lv1", AffCode: "ive", AccessToken: "t-ive", InviterId: inviter.Id}
	if err := DB.Create(invitee).Error; err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}

	var gotInviter int
	var gotQuota int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var e error
		gotInviter, gotQuota, e = GrantCommission(tx, invitee.Id, 100, 50000000,
			SourceTypeStripeCheckout, "trade-nocfg")
		return e
	})
	if err != nil {
		t.Fatalf("配置缺失应降级而非报错: %v", err)
	}
	if gotInviter != 0 || gotQuota != 0 {
		t.Errorf("got (%d, %d), want (0, 0)", gotInviter, gotQuota)
	}
}
