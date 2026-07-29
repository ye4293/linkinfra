package model

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

// 返现记录的来源渠道。用字符串而非枚举整数，为加密货币等新渠道
// 预留扩展位而不需要数据迁移。
const (
	SourceTypeStripeCheckout = "stripe_checkout" // Stripe Checkout 链路（model/topup.go）
	SourceTypeStripeCharge   = "stripe_charge"   // Stripe 套餐链路（model/charge_order.go）
)

// 返现记录状态
const (
	AffCommissionStatusGranted  = 1 // 已发放
	AffCommissionStatusReversed = 2 // 已冲正（对应充值被退款）
)

// AffCommissionRecord 邀请返现明细。每一笔返现都有一条记录，用于对账与展示。
//
// Rate 与 InviterGroup 是快照：后台改了比例不影响已发放记录的解释，
// 历史记录永远可复现当时的计算过程。
// 用户名也是快照，用户改名后对账不断链（用户 id 同时保留，两者互补）。
type AffCommissionRecord struct {
	Id              int    `json:"id" gorm:"primaryKey;autoIncrement"`
	InviterId       int    `json:"inviter_id" gorm:"index;not null"`
	InviteeId       int    `json:"invitee_id" gorm:"index;not null"`
	InviterUsername string `json:"inviter_username" gorm:"type:varchar(64)"`
	InviteeUsername string `json:"invitee_username" gorm:"type:varchar(64)"`
	SourceType      string `json:"source_type" gorm:"type:varchar(32)"`
	// SourceNo 是幂等的核心。Stripe 会重放 webhook，这个唯一索引是
	// 防重复发放的最后一道保险，比事务本身更重要。
	SourceNo        string  `json:"source_no" gorm:"type:varchar(128);uniqueIndex;not null"`
	TopupAmount     float64 `json:"topup_amount" gorm:"type:decimal(20,6)"` // 被邀请人实付金额
	TopupQuota      int64   `json:"topup_quota"`                            // 换算后的充值 quota
	Rate            float64 `json:"rate" gorm:"type:decimal(5,4)"`          // 比例快照
	InviterGroup    string  `json:"inviter_group" gorm:"type:varchar(32)"`  // 等级快照
	CommissionQuota int64   `json:"commission_quota"`                       // 实发返现 quota
	Status          int     `json:"status" gorm:"default:1;index"`
	// ReversedQuota 实际扣回的额度，可能小于 CommissionQuota——
	// 冲正时邀请人余额不足则扣到 0 为止，差额是运营的真实损失，必须可查。
	ReversedQuota int64 `json:"reversed_quota" gorm:"default:0"`
	CreatedAt     int64 `json:"created_at" gorm:"bigint;index"`
	ReversedAt    int64 `json:"reversed_at" gorm:"bigint;default:0"`
}

// GetAffCommissionRecordBySourceNo 按来源单号查询返现记录。
// 记录不存在时返回 (nil, nil)——调用方据此判断「那笔充值本来没有返现」，
// 这是正常分支而非故障。
func GetAffCommissionRecordBySourceNo(sourceNo string) (*AffCommissionRecord, error) {
	if sourceNo == "" {
		return nil, nil
	}
	var record AffCommissionRecord
	err := DB.Where("source_no = ?", sourceNo).First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// GetAffCommissionSummary 汇总某个邀请人的累计返现额与有效返现笔数。
// 已冲正（退款）的记录不计入。
func GetAffCommissionSummary(inviterId int) (totalQuota int64, count int64, err error) {
	if inviterId <= 0 {
		return 0, 0, nil
	}

	// 两次查询各自新建链式调用：GORM 的 *gorm.DB 在执行过终结方法
	// （Count / Scan 等）后会携带残留状态，复用同一个 tx 变量会让
	// 第二次查询带上第一次的 SELECT 子句。
	if err = DB.Model(&AffCommissionRecord{}).
		Where("inviter_id = ? AND status = ?", inviterId, AffCommissionStatusGranted).
		Count(&count).Error; err != nil {
		return 0, 0, err
	}

	// SUM 在无匹配行时返回 NULL，用 COALESCE 兜成 0，否则 Scan 到 int64 会失败
	if err = DB.Model(&AffCommissionRecord{}).
		Where("inviter_id = ? AND status = ?", inviterId, AffCommissionStatusGranted).
		Select("COALESCE(SUM(commission_quota), 0)").
		Scan(&totalQuota).Error; err != nil {
		return 0, count, err
	}

	return totalQuota, count, nil
}

// isDuplicateKeyError 判断是否为唯一键冲突。
//
// GORM 的 gorm.ErrDuplicatedKey 需要在 gorm.Config 里开启 TranslateError，
// 本项目未开启，因此只能按驱动的错误文本判断。三种数据库的措辞都覆盖到。
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || // sqlite / postgres
		strings.Contains(msg, "constraint failed") || // sqlite 另一种措辞
		strings.Contains(msg, "duplicate key") || // postgres
		strings.Contains(msg, "duplicate entry") // mysql
}

// GrantCommission 在给定事务内为被邀请人的一笔充值发放邀请返现。
//
// 必须在充值入账的同一事务内调用，以保证「入账 + 返现 + 明细」原子。
// 返回 (inviterId, commissionQuota, error)；无需返现时返回 (0, 0, nil)。
//
// 幂等：source_no 上有唯一索引。Stripe 会重放 webhook，重复调用会在
// INSERT 处被挡住并按成功返回，不会二次加额。
//
// 错误处理：除唯一键冲突外的 DB 错误一律返回 err，从而回滚整笔充值。
// 这看似激进，但是唯一正确的选择 —— Stripe 会重试 webhook，重试时靠
// source_no 唯一索引保证不重复入账，最终一致。反之若「返现失败只记 log
// 不阻塞充值」，返现就会静默丢钱，事后只能靠人工对账捞回。
//
// 唯一的例外是邀请人分组配置缺失：此时降级为「不返现」而非报错，
// 避免分组表的异常阻塞充值入账。
//
// topupAmount 取用户实付金额，不取扣手续费后的净额：对用户承诺「充值额的
// N%」必须字面成立，毛利通过调低 commission_rate 控制，而不是隐藏基数。
func GrantCommission(tx *gorm.DB, inviteeId int, topupAmount float64,
	topupQuota int64, sourceType, sourceNo string) (int, int64, error) {

	if inviteeId <= 0 || topupAmount <= 0 || sourceNo == "" {
		return 0, 0, nil
	}

	// 不用 Select 挑列：group 是 SQL 保留字，整行读取避免引号处理
	var invitee User
	if err := tx.Where("id = ?", inviteeId).First(&invitee).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	inviterId := invitee.InviterId
	if inviterId <= 0 {
		return 0, 0, nil
	}
	// 自邀请：理论上不可能，但 DB 被手工改过或未来新增绑定入口时会出现
	if inviterId == inviteeId {
		logger.SysError(fmt.Sprintf("user %d has itself as inviter, skipping commission", inviteeId))
		return 0, 0, nil
	}

	var inviter User
	if err := tx.Where("id = ?", inviterId).First(&inviter).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 邀请人已被删除
			return 0, 0, nil
		}
		return 0, 0, err
	}

	gc, err := GetGroupConfigByKeyTx(tx, inviter.Group)
	if err != nil {
		// 只有「这个分组确实没配」才降级为不返现（手工改过 DB 留下的野分组、
		// 运营还没给该等级建配置等），此时不该阻塞充值入账。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			logger.SysError(fmt.Sprintf(
				"group config %q not found for inviter %d, skipping commission",
				inviter.Group, inviterId))
			return 0, 0, nil
		}
		// 其余是真实的 DB 故障（表不存在、连接断开……），必须返回让事务回滚。
		// 尤其在 PostgreSQL 上：事务内任何一条语句失败后整个事务进入 aborted
		// 状态，后续语句全部报 "current transaction is aborted"。吞掉这个错误
		// 只会让调用方继续在一个已废的事务里做事，最终以一个与根因无关的错误
		// 失败，排查时完全找不到方向。
		return 0, 0, err
	}
	if gc.CommissionRate <= 0 {
		return 0, 0, nil
	}

	// 用 math.Round 而非截断，理由同 AmountToQuota：float64 的乘积常落在
	// 真值下方一点点，直接截断会单向少给邀请人。
	commissionQuota := int64(math.Round(topupAmount * gc.CommissionRate * config.QuotaPerUnit))
	if commissionQuota <= 0 {
		return 0, 0, nil
	}

	record := &AffCommissionRecord{
		InviterId:       inviterId,
		InviteeId:       inviteeId,
		InviterUsername: inviter.Username,
		InviteeUsername: invitee.Username,
		SourceType:      sourceType,
		SourceNo:        sourceNo,
		TopupAmount:     topupAmount,
		TopupQuota:      topupQuota,
		Rate:            gc.CommissionRate,
		InviterGroup:    inviter.Group,
		CommissionQuota: commissionQuota,
		Status:          AffCommissionStatusGranted,
		CreatedAt:       helper.GetTimestamp(),
	}
	if err := tx.Create(record).Error; err != nil {
		if isDuplicateKeyError(err) {
			// webhook 重放，已发放过
			return 0, 0, nil
		}
		return 0, 0, err
	}

	// 不能用 IncreaseUserQuota：它走全局 DB 且受 BatchUpdateEnabled 影响，
	// 会脱离当前事务
	if err := tx.Model(&User{}).Where("id = ?", inviterId).Updates(map[string]interface{}{
		"quota":      gorm.Expr("quota + ?", commissionQuota),
		"gift_quota": gorm.Expr("gift_quota + ?", commissionQuota),
	}).Error; err != nil {
		return 0, 0, err
	}

	return inviterId, commissionQuota, nil
}

// ReverseCommission 冲正一笔已发放的返现（对应充值被退款）。
//
// 必须在改单状态的同一事务内调用。
// 返回 (inviterId, actualReversedQuota, error)；无需冲正时返回 (0, 0, nil)。
//
// 余额不足时扣到 0 为止，绝不产生负余额 —— 邀请人可能已经把返现花掉了，
// 强行扣成负数会让他后续所有请求都被拒。差额（commission_quota -
// reversed_quota）是运营的真实损失，记在明细里并告警，可事后查账。
//
// 幂等：status != Granted 时直接返回，重复冲正不会二次扣款。
func ReverseCommission(tx *gorm.DB, sourceNo string) (int, int64, error) {
	if sourceNo == "" {
		return 0, 0, nil
	}

	var record AffCommissionRecord
	if err := tx.Where("source_no = ?", sourceNo).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 那笔充值本来就没有返现（无邀请人、比例为 0 等）
			return 0, 0, nil
		}
		return 0, 0, err
	}

	if record.Status != AffCommissionStatusGranted {
		// 已冲正过
		return 0, 0, nil
	}

	var inviter User
	if err := tx.Where("id = ?", record.InviterId).First(&inviter).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 邀请人已注销：只标记记录，无处可扣
			if markErr := markCommissionReversed(tx, &record, 0); markErr != nil {
				return 0, 0, markErr
			}
			logger.SysError(fmt.Sprintf(
				"commission reversal for %s: inviter %d no longer exists, %d quota unrecoverable",
				sourceNo, record.InviterId, record.CommissionQuota))
			return record.InviterId, 0, nil
		}
		return 0, 0, err
	}

	actualReverse := record.CommissionQuota
	if inviter.Quota < actualReverse {
		actualReverse = inviter.Quota
	}
	if actualReverse < 0 {
		actualReverse = 0
	}

	if actualReverse > 0 {
		// gift_quota 与 quota 同步递减。gift_quota 平时只增，
		// 退款冲正是唯一的例外 —— 那笔钱事实上没有发生。
		if err := tx.Model(&User{}).Where("id = ?", record.InviterId).
			Updates(map[string]interface{}{
				"quota":      gorm.Expr("quota - ?", actualReverse),
				"gift_quota": gorm.Expr("gift_quota - ?", actualReverse),
			}).Error; err != nil {
			return 0, 0, err
		}
	}

	if err := markCommissionReversed(tx, &record, actualReverse); err != nil {
		return 0, 0, err
	}

	if actualReverse < record.CommissionQuota {
		logger.SysError(fmt.Sprintf(
			"commission reversal for %s incomplete: reversed %d of %d from inviter %d (balance insufficient)",
			sourceNo, actualReverse, record.CommissionQuota, record.InviterId))
	}

	return record.InviterId, actualReverse, nil
}

// markCommissionReversed 把记录标记为已冲正。
func markCommissionReversed(tx *gorm.DB, record *AffCommissionRecord, reversedQuota int64) error {
	return tx.Model(&AffCommissionRecord{}).Where("id = ?", record.Id).
		Updates(map[string]interface{}{
			"status":         AffCommissionStatusReversed,
			"reversed_quota": reversedQuota,
			"reversed_at":    helper.GetTimestamp(),
		}).Error
}
