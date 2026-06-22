package model

import (
	"errors"
	"fmt"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"gorm.io/gorm"
)

type Redemption struct {
	Id           int    `json:"id"`
	UserId       int    `json:"user_id"`
	Key          string `json:"key" gorm:"type:char(32);uniqueIndex"`
	Status       int    `json:"status" gorm:"default:1"`
	Name         string `json:"name" gorm:"index"`
	Quota        int64  `json:"quota" gorm:"bigint;default:100"`
	CreatedTime  int64  `json:"created_time" gorm:"bigint"`
	RedeemedTime int64  `json:"redeemed_time" gorm:"bigint"`
	Count        int    `json:"count" gorm:"-:all"` // only for api request
}

func GetAllRedemptions(startIdx int, num int) ([]*Redemption, error) {
	var redemptions []*Redemption
	var err error
	err = DB.Order("id desc").Limit(num).Offset(startIdx).Find(&redemptions).Error
	return redemptions, err
}

func GetAllRedemptionsAndCount(page int, pageSize int) (redemptions []*Redemption, total int64, err error) {
	// 首先计算Redemption记录的总数
	err = DB.Model(&Redemption{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算起始索引，基于page和pageSize。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 获取当前页面的Redemption列表
	err = DB.Order("id desc").Limit(pageSize).Offset(offset).Find(&redemptions).Error
	if err != nil {
		return nil, total, err
	}

	// 返回Redemption列表、总数以及可能的错误信息
	return redemptions, total, nil
}

func SearchRedemptionsAndCount(keyword string, status *int, page int, pageSize int) (redemptions []*Redemption, total int64, err error) {
	// 用于LIKE查询的关键词格式
	likeKeyword := "%" + keyword + "%"

	// 构建基础查询
	baseQuery := DB.Model(&Redemption{}).Where("id = ? OR name LIKE ?", keyword, likeKeyword)

	// 如果status不为nil，加入status作为查询条件
	if status != nil {
		baseQuery = baseQuery.Where("status = ?", *status)
	}

	// 计算满足条件的Redemption记录总数
	err = baseQuery.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算分页的偏移量
	offset := (page - 1) * pageSize

	// 获取满足条件的Redemption列表的子集，并应用分页参数
	// 添加Order方法以按照id字段进行降序排列
	err = baseQuery.Order("id DESC").Offset(offset).Limit(pageSize).Find(&redemptions).Error
	if err != nil {
		return nil, total, err
	}

	// 返回Redemption列表的子集、总数以及可能的错误信息
	return redemptions, total, nil
}

func SearchRedemptions(keyword string) (redemptions []*Redemption, err error) {
	err = DB.Where("id = ? or name LIKE ?", keyword, keyword+"%").Find(&redemptions).Error
	return redemptions, err
}

func GetRedemptionById(id int) (*Redemption, error) {
	if id == 0 {
		return nil, errors.New("id is required")
	}
	redemption := Redemption{Id: id}
	var err error = nil
	err = DB.First(&redemption, "id = ?", id).Error
	return &redemption, err
}

func Redeem(key string, userId int) (quota int64, err error) {
	if key == "" {
		return 0, errors.New("redemption code is required")
	}
	if userId == 0 {
		return 0, errors.New("invalid user id")
	}
	redemption := &Redemption{}

	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Set("gorm:query_option", "FOR UPDATE").Where(keyCol+" = ?", key).First(redemption).Error
		if err != nil {
			return errors.New("invalid redemption code")
		}
		if redemption.Status != common.RedemptionCodeStatusEnabled {
			return errors.New("redemption code has already been used")
		}
		err = tx.Model(&User{}).Where("id = ?", userId).Update("quota", gorm.Expr("quota + ?", redemption.Quota)).Error
		if err != nil {
			return err
		}
		redemption.RedeemedTime = helper.GetTimestamp()
		redemption.Status = common.RedemptionCodeStatusUsed
		err = tx.Save(redemption).Error
		return err
	})
	if err != nil {
		return 0, errors.New("redemption failed: " + err.Error())
	}
	RecordLog(userId, LogTypeTopup, fmt.Sprintf("redemption code top-up: %s", common.LogQuota(redemption.Quota)))
	return redemption.Quota, nil
}

func (redemption *Redemption) Insert() error {
	var err error
	err = DB.Create(redemption).Error
	return err
}

func (redemption *Redemption) SelectUpdate() error {
	// This can update zero values
	return DB.Model(redemption).Select("redeemed_time", "status").Updates(redemption).Error
}

// Update Make sure your token's fields is completed, because this will update non-zero values
func (redemption *Redemption) Update() error {
	var err error
	err = DB.Model(redemption).Select("name", "status", "quota", "redeemed_time").Updates(redemption).Error
	return err
}

func (redemption *Redemption) Delete() error {
	var err error
	err = DB.Delete(redemption).Error
	return err
}

func DeleteRedemptionById(id int) (err error) {
	if id == 0 {
		return errors.New("id is required")
	}
	redemption := Redemption{Id: id}
	err = DB.Where(redemption).First(&redemption).Error
	if err != nil {
		return err
	}
	return redemption.Delete()
}

func DeleteRedemptionsByIds(ids []int) error {
	// 检查ids是否有效
	if len(ids) == 0 {
		return errors.New("ids list is empty")
	}

	// 构造查询条件，只删除ID在ids列表中的redemption
	result := DB.Where("id IN ?", ids).Delete(&Redemption{})
	if result.Error != nil {
		return result.Error
	}

	return nil
}
