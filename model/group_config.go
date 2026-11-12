package model

import (
	"sort"

	"github.com/songquanpeng/one-api/common"
	"gorm.io/gorm"
)

// GroupConfig 分组等级配置表
type GroupConfig struct {
	ID          int     `json:"id" gorm:"primaryKey;autoIncrement"`
	GroupKey    string  `json:"group_key" gorm:"type:varchar(32);uniqueIndex;not null"` // 对应 GroupRatio 的 key
	DisplayName string  `json:"display_name" gorm:"type:varchar(64);not null"`          // 显示名称，如 "Start"
	Discount    float64 `json:"discount" gorm:"type:decimal(4,2);default:1.0"`          // 等级折扣倍率
	SortOrder   int     `json:"sort_order" gorm:"default:0"`                            // 显示排序
	Description string  `json:"description" gorm:"type:varchar(255)"`                   // 等级描述
	// CommissionRate 该等级的邀请返现比例，[0, 1]。默认 0 = 该等级不返现。
	// 取值时看的是「邀请人」自己的等级，而非被邀请人的。
	CommissionRate float64 `json:"commission_rate" gorm:"type:decimal(5,4);default:0"`
	// UpgradeThreshold 升到本等级所需的累计真实充值 quota（对应 users.topup_quota）。
	// 0 表示无门槛。等级判定取「满足门槛的最高等级」，并列时取 SortOrder 较小者。
	UpgradeThreshold int64 `json:"upgrade_threshold" gorm:"type:bigint;default:0"`
}

func GetAllGroupConfigs() (configs []GroupConfig, err error) {
	err = DB.Order("sort_order asc, id asc").Find(&configs).Error
	return configs, err
}

func GetGroupConfigByKey(key string) (*GroupConfig, error) {
	var config GroupConfig
	err := DB.Where("group_key = ?", key).First(&config).Error
	return &config, err
}

func CreateGroupConfig(config *GroupConfig) error {
	return DB.Create(config).Error
}

func UpdateGroupConfig(config *GroupConfig) error {
	return DB.Save(config).Error
}

func DeleteGroupConfigByID(id int) error {
	return DB.Delete(&GroupConfig{}, id).Error
}

func GetGroupConfigByID(id int) (*GroupConfig, error) {
	var config GroupConfig
	err := DB.First(&config, id).Error
	return &config, err
}

// defaultUpgradeThresholds 各等级的默认升级门槛（单位 quota）。
// 仅供全新库使用，已有配置保持不变。
var defaultUpgradeThresholds = map[string]int64{
	"Start":      0,
	"Build":      50 * 500000,
	"Scale":      300 * 500000,
	"Enterprise": 1000 * 500000,
}

// InitGroupConfigs 在表为空时，从现有 GroupRatio 初始化默认配置。
//
// 默认档位按门槛排序，其余 key 排在后面按字典序排列。
func InitGroupConfigs(db *gorm.DB) error {
	var count int64
	if err := db.Model(&GroupConfig{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	keys := make([]string, 0, len(common.GroupRatio))
	for key := range common.GroupRatio {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aKnown := defaultUpgradeThresholds[keys[i]]
		b, bKnown := defaultUpgradeThresholds[keys[j]]
		if aKnown != bKnown {
			return aKnown
		}
		if aKnown && a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})

	for order, key := range keys {
		config := GroupConfig{
			GroupKey:    key,
			DisplayName: key,
			Discount:    common.GroupRatio[key],
			SortOrder:   order,
			// CommissionRate 保持零值：返现默认全局关闭，由管理员在后台逐级开启
			UpgradeThreshold: defaultUpgradeThresholds[key],
		}
		if err := db.Create(&config).Error; err != nil {
			return err
		}
	}
	return nil
}

// GetGroupConfigsByThresholdDesc 按升级门槛降序返回全部分组配置。
// 门槛并列时按 sort_order 升序，即并列时较低等级排在前面——
// 等级判定取第一个满足门槛的分组，这个顺序保证运营新增分组时
// 若忘记设门槛（默认 0），用户会落到入门档而不是被拉到新分组。
func GetGroupConfigsByThresholdDesc() (configs []GroupConfig, err error) {
	err = DB.Order("upgrade_threshold desc, sort_order asc, id asc").Find(&configs).Error
	return configs, err
}

// GetGroupConfigByKeyTx 在给定事务内按 key 查询分组配置。
// 返现发放必须与充值入账同事务，因此不能复用走全局 DB 的 GetGroupConfigByKey。
func GetGroupConfigByKeyTx(tx *gorm.DB, key string) (*GroupConfig, error) {
	var config GroupConfig
	err := tx.Where("group_key = ?", key).First(&config).Error
	return &config, err
}

// GetDefaultGroup 返回当前配置的入门档；空表或查询失败时回退到 Start。
func GetDefaultGroup() string {
	return getDefaultGroupTx(DB)
}

func getDefaultGroupTx(tx *gorm.DB) string {
	if tx == nil {
		return "Start"
	}
	var entry GroupConfig
	// NewDB 清除创建语句的条件，但保留连接和事务。嵌套事务的保存点
	// 让 PostgreSQL 查询失败后仍可创建用户，避免整个创建事务被标记为失败。
	err := tx.Session(&gorm.Session{NewDB: true}).Transaction(func(query *gorm.DB) error {
		return query.Order("upgrade_threshold asc, sort_order asc, id asc").Limit(1).Find(&entry).Error
	})
	if err != nil || entry.GroupKey == "" {
		return "Start"
	}
	return entry.GroupKey
}
