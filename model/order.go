package model

import (
	"errors"
	"fmt"
	"sync"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/message"
	"gorm.io/gorm"
)

type Order struct {
	Id                 int     `json:"id"`
	Username           string  `json:"username" gorm:"index" validate:"max=12"`
	UserId             int     `json:"user_id" gorm:"type:varchar(20);index"`
	Type               string  `json:"type"`
	Uuid               string  `json:"uuid" gorm:"type:varchar(100);index"`
	Status             int     `json:"status" gorm:"default:1"`
	Ticker             string  `json:"ticker" gorm:"type:varchar(20)"`
	AddressOut         string  `json:"address_out" gorm:"type:varchar(100);default:''"`
	AddressIn          string  `json:"address_in" gorm:"type:varchar(100);default:''"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	UpdatedTime        int64   `json:"updated_time" gorm:"bigint"`
	FeeCoin            float64 `json:"fee_coin" gorm:"type:decimal(20,6);default:0"`
	ValueCoin          float64 `json:"value_coin" gorm:"type:decimal(20,6);default:0"`
	ValueForwardedCoin float64 `json:"value_forwarded_coin" gorm:"type:decimal(20,6);default:0"`
	Extra              string  `json:"extra" gorm:"type:text"`
}

var lock sync.Mutex

func CreateOrUpdateOrder(response CryptCallbackResponse, username string) error {
	lock.Lock()
	defer lock.Unlock()
	status := CryptResponseResult[response.Result]
	//userId,err:=Decrypt(response.UserId)
	//先查询订单
	var order Order
	if err := DB.Where("uuid = ?", response.Uuid).First(&order).Error; err != nil {
		//如果没有订单
		if errors.Is(err, gorm.ErrRecordNotFound) {
			//支付成功
			if status == 3 {
				var addAmount float64
				if err := DB.Transaction(func(tx *gorm.DB) error {
					// 创建一笔订单
					if err = DB.Create(&Order{
						UserId:             response.UserId,
						Username:           username,
						Type:               "Crypto",
						Uuid:               response.Uuid,
						Status:             status,
						Ticker:             response.Coin,
						AddressOut:         response.AddressOut,
						AddressIn:          response.AddressIn,
						FeeCoin:            response.FeeCoin,
						ValueCoin:          response.ValueCoin,
						ValueForwardedCoin: response.ValueForwardedCoin,
						CreatedTime:        helper.GetTimestamp(),
						UpdatedTime:        helper.GetTimestamp(),
					}).Error; err != nil {
						return err
					}

					if err = DB.Create(&Bill{
						Username:  username,
						UserId:    response.UserId,
						Type:      "Crypto",
						CreatedAt: helper.GetTimestamp(),
						Amount:    response.ValueCoin,
						SourceId:  response.Uuid,
						Status:    status,
					}).Error; err != nil {
						return err
					}

					//更新余额 待定手续费和用户组别的变更
					// 这里必须是赋值而非 := 声明：外层 addAmount 在事务提交后
					// 要传给 AfterChargeSuccess 发送充值成功邮件。用 := 会新建
					// 一个闭包内变量，导致邮件金额恒为 0。
					addAmount = response.ValueForwardedCoin
					err = IncreaseUserQuota(response.UserId, AmountToQuota(addAmount))
					if err != nil {
						return err
					}
					// 返回 nil 提交事务
					return nil
				}); err != nil {
					return err
				}

				//crypt支付成功处理一下其它
				AfterChargeSuccess(response.UserId, addAmount)
			}

		} else {
			return err
		}

	} else {
		if order.Status < status {
			err = UpdateOrder(order.Uuid, Order{
				Status:      status,
				UpdatedTime: helper.GetTimestamp(),
			})
			if err != nil {
				return err
			}
			err = UpdateBill(order.Uuid, Bill{
				Status:    status,
				UpdatedAt: helper.GetTimestamp(),
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func AfterChargeSuccess(userId int, addAmount float64) {

	//send email and back message
	email, err := GetUserEmail(userId)
	if err != nil {
		logger.SysLog("failed to get user email")
		return
	}
	subject := fmt.Sprintf("%s's recharge notification email", config.SystemName)
	content := fmt.Sprintf("<p>Hello,You have successfully recharged %f$</p>"+"<p>Congratulations on getting one step closer to the AI world!</p>", addAmount)
	err = message.SendEmail(subject, email, content)
	if err != nil {
		return
	}

}

func UpdateOrder(uuid string, order Order) error {
	err := DB.Model(&Order{}).Where("uuid=?", uuid).Updates(order).Error
	if err != nil {
		return err
	}
	return nil
}

func GetAllBillsAndCount(page int, pageSize int, username string, startTimestamp int64, endTimestamp int64) (orders []*Order, total int64, err error) {
	tx := DB
	// 进一步根据提供的参数筛选日志
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_time >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_time <= ?", endTimestamp)
	}

	// 首先计算满足条件的总数
	err = tx.Model(&Order{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算起始索引。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 然后获取满足条件的日志数据
	err = tx.Order("id desc").Limit(pageSize).Offset(offset).Find(&orders).Error
	if err != nil {
		return nil, total, err
	}

	// 返回日志数据、总数以及错误信息
	return orders, total, nil

}

func GetUserBillsAndCount(page int, pageSize int, userId int, startTimestamp int64, endTimestamp int64) (orders []*Order, total int64, err error) {
	tx := DB
	// 进一步根据提供的参数筛选日志
	tx = tx.Where("user_id = ?", userId)

	if startTimestamp != 0 {
		tx = tx.Where("created_time >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_time <= ?", endTimestamp)
	}
	// 首先计算满足条件的总数
	err = tx.Model(&Order{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算起始索引。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 然后获取满足条件的日志数据
	err = tx.Order("id desc").Limit(pageSize).Offset(offset).Find(&orders).Error
	if err != nil {
		return nil, total, err
	}

	// 返回日志数据、总数以及错误信息
	return orders, total, nil
}
