package model

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

type Ability struct {
	Group     string `json:"group" gorm:"type:varchar(32);primaryKey;autoIncrement:false"`
	Model     string `json:"model" gorm:"primaryKey;autoIncrement:false"`
	ChannelId int    `json:"channel_id" gorm:"primaryKey;autoIncrement:false;index"`
	Enabled   bool   `json:"enabled"`
	Priority  *int64 `json:"priority" gorm:"bigint;default:0;index"`
}

func GetRandomSatisfiedChannel(group string, model string) (*Channel, error) {
	groupCol := "`group`"
	trueVal := "1"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
		trueVal = "true"
	}

	// 获取同优先级下所有可用的渠道及其权重
	var channels []Channel
	maxPrioritySubQuery := DB.Model(&Ability{}).Select("MAX(priority)").Where(groupCol+" = ? and model = ? and enabled = "+trueVal, group, model)

	err := DB.Table("channels").
		Joins("JOIN abilities ON channels.id = abilities.channel_id").
		// 用 groupCol 而非硬编码反引号：PG 的标识符引号是双引号，反引号
		// 是语法错误。上面 :24-29 已经备好方言分支，这里必须用上。
		//
		// enabled 传 Go 的 true 字面量而非 trueVal 字符串 —— trueVal 的
		// 设计意图是拼进 SQL 文本（见 :33 的 maxPrioritySubQuery），
		// 当绑定参数传给 boolean 列是靠隐式转型侥幸能过。
		Where("abilities."+groupCol+" = ? AND abilities.model = ? AND abilities.enabled = ? AND abilities.priority = (?)",
			group, model, true, maxPrioritySubQuery).
		Find(&channels).Error

	if err != nil {
		return nil, err
	}

	totalWeight := 0
	for _, channel := range channels {
		// 检查 weight 值，如果小于等于 0，则将其设置为 1
		weight := int(*channel.Weight)
		if weight <= 0 {
			weight = 1
		}
		totalWeight += weight
	}

	if totalWeight == 0 || len(channels) == 0 {
		return nil, errors.New("no channels available with the required priority and weight")
	}

	// 生成一个随机权重阈值。
	//
	// 不要写成 rand.New(rand.NewSource(time.Now().UnixNano()))：Windows 的
	// 时钟精度约 0.5~15ms，同一 tick 内的并发请求会拿到相同种子、算出相同的
	// 阈值，加权随机退化成固定选择，负载全压在同一个渠道上。
	// Go 1.20+ 的全局 rand 已自动随机播种、并发安全、且走无锁快路径。
	weightThreshold := rand.Intn(totalWeight) + 1

	currentWeight := 0
	for _, channel := range channels {
		// 同样地，检查并调整 weight 值
		weight := int(*channel.Weight)
		if weight <= 0 {
			weight = 1
		}
		currentWeight += weight
		if currentWeight >= weightThreshold {
			return &channel, nil
		}
	}

	return nil, errors.New("unable to select a channel based on weight")
}

func (channel *Channel) AddAbilities() error {
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilities := make([]Ability, 0, len(models_)*len(groups_))
	for _, model := range models_ {
		model = strings.TrimSpace(model) // 去除空格
		if model == "" {
			continue // 跳过空模型
		}
		for _, group := range groups_ {
			group = strings.TrimSpace(group) // 去除空格
			if group == "" {
				continue // 跳过空组
			}
			ability := Ability{
				Group:     group,
				Model:     model,
				ChannelId: channel.Id,
				Enabled:   channel.Status == common.ChannelStatusEnabled,
				Priority:  channel.Priority,
			}
			abilities = append(abilities, ability)
		}
	}

	// 分批插入以避免 "too many SQL variables" 错误
	// SQLite 默认限制为999个变量，每条记录5个字段，所以每批最多150条记录 (150 * 5 = 750 < 999)
	// MySQL 限制更高，但使用相同的批量大小保持兼容性
	batchSize := 150
	for i := 0; i < len(abilities); i += batchSize {
		end := i + batchSize
		if end > len(abilities) {
			end = len(abilities)
		}
		batch := abilities[i:end]
		if err := DB.Create(&batch).Error; err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) DeleteAbilities() error {
	return DB.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
}

// UpdateAbilities updates abilities of this channel.
// Make sure the channel is completed before calling this function.
func (channel *Channel) UpdateAbilities() error {
	// A quick and dirty way to update abilities
	// First delete all abilities of this channel
	err := channel.DeleteAbilities()
	if err != nil {
		return err
	}
	// Then add new abilities
	err = channel.AddAbilities()
	if err != nil {
		return err
	}
	return nil
}

// UpdateAbilityStatus 已废弃：请使用 UpdateChannelStatusById 确保数据一致性
// Deprecated: Use UpdateChannelStatusById instead to ensure data consistency
func UpdateAbilityStatus(channelId int, status bool) error {
	logger.SysError("WARNING: UpdateAbilityStatus is deprecated and may cause data inconsistency. Use UpdateChannelStatusById instead.")
	return DB.Model(&Ability{}).Where("channel_id = ?", channelId).Select("enabled").Update("enabled", status).Error
}

// CheckDataConsistency 检查并修复 channels 和 abilities 表的数据一致性
func CheckDataConsistency() error {
	// 先检查不一致的数量
	var inconsistentCount int64
	err := DB.Table("abilities a").
		Joins("JOIN channels c ON a.channel_id = c.id").
		// 用 false/true 而非 0/1：Ability.Enabled 是 Go bool → PG boolean，
		// PG 不允许 boolean 与 integer 比较（operator does not exist）。
		// 传 Go 字面量三库都认。
		Where("(c.status = ? AND a.enabled = ?) OR (c.status != ? AND a.enabled = ?)",
			common.ChannelStatusEnabled, false, common.ChannelStatusEnabled, true).
		Count(&inconsistentCount).Error

	if err != nil {
		logger.SysError("Failed to check data consistency: " + err.Error())
		return err
	}

	if inconsistentCount > 0 {
		logger.SysLog(fmt.Sprintf("Found %d inconsistent ability records, fixing...", inconsistentCount))

		// 修复不一致的数据 - 根据数据库类型使用不同语法
		var result *gorm.DB
		if common.UsingMySQL {
			// MySQL: UPDATE ... JOIN 语法，enabled 是 tinyint(1)，用 1/0
			result = DB.Exec(`
				UPDATE abilities a
				JOIN channels c ON a.channel_id = c.id
				SET a.enabled = CASE
					WHEN c.status = ? THEN 1
					ELSE 0
				END
				WHERE (c.status = ? AND a.enabled = 0) OR (c.status != ? AND a.enabled = 1)
			`, common.ChannelStatusEnabled, common.ChannelStatusEnabled, common.ChannelStatusEnabled)
		} else if common.UsingPostgreSQL {
			// PostgreSQL 与 MySQL 有三处不同，不能共用一条语句：
			// 1. PG 不支持 UPDATE t JOIN ...，只支持 UPDATE t SET ... FROM ...
			// 2. PG 的 SET 子句不允许表限定名（SET a.enabled = ... 是语法错误）
			// 3. abilities.enabled 是 boolean，不能赋 1/0、也不能与 0/1 比较
			result = DB.Exec(`
				UPDATE abilities
				SET enabled = (c.status = ?)
				FROM channels c
				WHERE c.id = abilities.channel_id
				  AND ((c.status = ? AND abilities.enabled = false)
					OR (c.status <> ? AND abilities.enabled = true))
			`, common.ChannelStatusEnabled, common.ChannelStatusEnabled, common.ChannelStatusEnabled)
		} else {
			// SQLite: 使用子查询语法
			result = DB.Exec(`
				UPDATE abilities 
				SET enabled = CASE 
					WHEN (SELECT status FROM channels WHERE channels.id = abilities.channel_id) = ? THEN 1
					ELSE 0
				END
				WHERE EXISTS (
					SELECT 1 FROM channels 
					WHERE channels.id = abilities.channel_id 
					AND ((channels.status = ? AND abilities.enabled = 0) OR (channels.status != ? AND abilities.enabled = 1))
				)
			`, common.ChannelStatusEnabled, common.ChannelStatusEnabled, common.ChannelStatusEnabled)
		}

		if result.Error != nil {
			logger.SysError("Failed to fix data consistency: " + result.Error.Error())
			return result.Error
		}

		logger.SysLog(fmt.Sprintf("Fixed %d ability records for data consistency", result.RowsAffected))
	} else {
		logger.SysLog("Data consistency check passed - no issues found")
	}

	return nil
}

// SyncChannelAbilities 同步指定渠道的 abilities 状态
func SyncChannelAbilities(channelId int) error {
	var channel Channel
	err := DB.First(&channel, channelId).Error
	if err != nil {
		return fmt.Errorf("channel not found: %w", err)
	}

	enabled := channel.Status == common.ChannelStatusEnabled
	result := DB.Model(&Ability{}).Where("channel_id = ?", channelId).Update("enabled", enabled)

	if result.Error != nil {
		logger.SysError(fmt.Sprintf("Failed to sync abilities for channel %d: %s", channelId, result.Error.Error()))
		return result.Error
	}

	logger.SysLog(fmt.Sprintf("Synced %d abilities for channel %d (enabled=%v)", result.RowsAffected, channelId, enabled))
	return nil
}

func FindEnabledModelsByGroup(group string) ([]string, error) {
	var models []string

	// group 是 SQL 保留字：PG 用双引号，MySQL/sqlite 用反引号。
	// 与 GetRandomSatisfiedChannel :24-29 同构。
	groupCol := "`group`"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
	}

	// 构建查询：按 group 取去重后的 model，并按各 model 的最高 priority 降序。
	//
	// 不能写成 SELECT DISTINCT model ... ORDER BY priority DESC —— PG 会报
	// "for SELECT DISTINCT, ORDER BY expressions must appear in select list"，
	// 而 MySQL/sqlite 宽容通过，属于只在 PG 上才炸的写法。
	// 改用 GROUP BY model + ORDER BY MAX(priority)，三库都合法且语义等价。
	err := DB.Model(&Ability{}).
		Select("model").
		Where(groupCol+" = ? AND enabled = ?", group, true).
		Group("model").
		Order("MAX(priority) DESC").
		Pluck("model", &models).Error

	if err != nil {
		return nil, err
	}

	return models, nil
}
