package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/blacklist"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

// User if you add sensitive fields, don't forget to clean them in setupLogin function.
// Otherwise, the sensitive information will be saved on local storage in plain text!
type User struct {
	Id                      int    `json:"id"`
	Username                string `json:"username" gorm:"unique;index" validate:"max=12"`
	Password                string `json:"password" gorm:"not null;" validate:"min=8,max=20"`
	DisplayName             string `json:"display_name" gorm:"index" validate:"max=20"`
	Role                    int    `json:"role" gorm:"type:int;default:1"`   // admin, util
	Status                  int    `json:"status" gorm:"type:int;default:1"` // enabled, disabled
	Email                   string `json:"email" gorm:"index" validate:"max=50"`
	GitHubId                string `json:"github_id" gorm:"column:github_id;index"`
	GoogleId                string `json:"google_id" gorm:"column:google_id;index"`
	WeChatId                string `json:"wechat_id" gorm:"column:wechat_id;index"`
	VerificationCode        string `json:"verification_code" gorm:"-:all"`                                    // this field is only for Email verification, don't save it to database!
	AccessToken             string `json:"access_token" gorm:"type:char(32);column:access_token;uniqueIndex"` // this token is for system management
	Quota                   int64  `json:"quota" gorm:"type:int;default:0"`
	UsedQuota               int64  `json:"used_quota" gorm:"type:int;default:0;column:used_quota"` // used quota
	RequestCount            int    `json:"request_count" gorm:"type:int;default:0;"`               // request number
	Group                   string `json:"group" gorm:"type:varchar(32);default:'Lv1"`
	AffCode                 string `json:"aff_code" gorm:"type:varchar(32);column:aff_code;uniqueIndex"`
	InviterId               int    `json:"inviter_id" gorm:"type:int;column:inviter_id;index"`
	UserRemindThreshold     int64  `json:"user_remind_threshold"`
	UserLastNoticeTime int64 `json:"user_last_notice_time" gorm:"default:0"`
	ChannelRatios           string `json:"channel_ratios" gorm:"type:text"`
}

// GetChannelRatiosMap 解析 ChannelRatios JSON 为 map[channelType]ratio。
// 空串、解析失败均返回空 map，调用方不需要再判 nil。
func (user *User) GetChannelRatiosMap() map[int]float64 {
	result := map[int]float64{}
	if user.ChannelRatios == "" {
		return result
	}
	raw := map[string]float64{}
	if err := json.Unmarshal([]byte(user.ChannelRatios), &raw); err != nil {
		return result
	}
	for k, v := range raw {
		if v <= 0 {
			continue
		}
		ct, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		result[ct] = v
	}
	return result
}

// SetChannelRatiosMap 序列化 map 到 ChannelRatios。空 map 置空串。
func (user *User) SetChannelRatiosMap(m map[int]float64) error {
	if len(m) == 0 {
		user.ChannelRatios = ""
		return nil
	}
	raw := make(map[string]float64, len(m))
	for k, v := range m {
		if v <= 0 {
			continue
		}
		raw[strconv.Itoa(k)] = v
	}
	if len(raw) == 0 {
		user.ChannelRatios = ""
		return nil
	}
	bs, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	user.ChannelRatios = string(bs)
	return nil
}

// GetChannelRatio 取指定渠道类型的用户折扣，缺失/非正数返回 1.0。
func (user *User) GetChannelRatio(channelType int) float64 {
	if channelType <= 0 {
		return 1.0
	}
	ratios := user.GetChannelRatiosMap()
	if r, ok := ratios[channelType]; ok && r > 0 {
		return r
	}
	return 1.0
}

func GetMaxUserId() int {
	var user User
	DB.Last(&user)
	return user.Id
}

func GetCurrentPageUsersAndCount(page int, pageSize int) (users []*User, total int64, err error) {
	// 首先计算用户总数
	err = DB.Model(&User{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算起始索引，基于page和pageSize。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 获取当前页面的用户列表，忽略密码字段
	err = DB.Order("id desc").Limit(pageSize).Offset(offset).Omit("password").Find(&users).Error
	if err != nil {
		return nil, total, err
	}

	// 返回用户列表、总数以及可能的错误信息
	return users, total, nil
}

func GetAllUsers(startIdx int, num int) (users []*User, err error) {
	err = DB.Order("id desc").Limit(num).Offset(startIdx).Omit("password").Where("status != ?", common.UserStatusDeleted).Find(&users).Error
	return users, err
}

func SearchUsersAndCount(keyword string, page int, pageSize int, status *int) (users []*User, total int64, err error) {
	likeKeyword := "%" + keyword + "%"
	query := DB.Omit("password")

	if !common.UsingPostgreSQL {
		// Add status to the query condition if not nil
		query = query.Where("id = ? or username LIKE ? or email LIKE ? or display_name LIKE ?", keyword, likeKeyword, likeKeyword, likeKeyword)
		if status != nil {
			query = query.Where("status = ?", *status)
		}
	} else {
		// Add status to the query condition for PostgreSQL if not nil
		query = query.Where("username LIKE ? or email LIKE ? or display_name LIKE ?", likeKeyword, likeKeyword, likeKeyword)
		if status != nil {
			query = query.Where("status = ?", *status)
		}
	}

	// 先计算总数
	err = query.Model(&User{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 计算偏移量
	offset := (page - 1) * pageSize

	// 分页查询
	err = query.Order("id DESC").Offset(offset).Limit(pageSize).Find(&users).Error
	return users, total, err
}

func SearchUsers(keyword string) (users []*User, err error) {
	if !common.UsingPostgreSQL {
		err = DB.Omit("password").Where("id = ? or username LIKE ? or email LIKE ? or display_name LIKE ?", keyword, keyword+"%", keyword+"%", keyword+"%").Find(&users).Error
	} else {
		err = DB.Omit("password").Where("username LIKE ? or email LIKE ? or display_name LIKE ?", keyword+"%", keyword+"%", keyword+"%").Find(&users).Error
	}
	return users, err
}

func GetUserById(id int, selectAll bool) (*User, error) {
	if id == 0 {
		return nil, errors.New("id is required")
	}
	user := User{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(&user, "id = ?", id).Error
	} else {
		err = DB.Omit("password").First(&user, "id = ?", id).Error
	}
	return &user, err
}

func GetUserByUsername(username string, selectAll bool) (*User, error) {
	if username == "" {
		return nil, errors.New("username is required")
	}
	var user User
	err := DB.Where("username = ?", username).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func GetUserIdByAffCode(affCode string) (int, error) {
	if affCode == "" {
		return 0, errors.New("affCode is required")
	}
	var user User
	err := DB.Select("id").First(&user, "aff_code = ?", affCode).Error
	return user.Id, err
}

func GetUserByEmail(email string) (*User, error) {
	if email == "" {
		return nil, errors.New("email is required")
	}
	var user User
	err := DB.Where("email = ?", email).First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func GetUserByEmail2(email string) (user *User, err error) {
	user = &User{}
	err = DB.Where("email = ?", email).First(user).Error
	return user, err
}

func DeleteUserById(id int) (err error) {
	if id == 0 {
		return errors.New("id is required")
	}
	user := User{Id: id}
	return user.Delete()
}

func DeleteUsersByIds(ids []int) error {

	// 假设有一个全局的DB对象用于数据库操作
	// 这里使用了GORM的Delete方法，其中`id IN (?)`是SQL语句的一部分，用于匹配IDs列表中的任何ID
	result := DB.Where("id IN (?)", ids).Delete(&User{})
	if result.Error != nil {
		return result.Error
	}

	// 如果你需要检查是否所有的用户都被成功删除（比如有些ID可能不存在），你可以检查result.RowsAffected
	if result.RowsAffected != int64(len(ids)) {
		return errors.New("not all specified users were deleted")
	}

	return nil
}

func (user *User) Insert(inviterId int) error {
	var err error
	if user.Password != "" {
		user.Password, err = common.Password2Hash(user.Password)
		if err != nil {
			return err
		}
	}
	user.Quota = config.QuotaForNewUser
	user.AccessToken = helper.GetUUID()
	user.AffCode = helper.GetRandomString(4)
	result := DB.Create(user)
	if result.Error != nil {
		return result.Error
	}
	if config.QuotaForNewUser > 0 {
		RecordLog(user.Id, LogTypeSystem, fmt.Sprintf("新用户注册赠送 %s", common.LogQuota(config.QuotaForNewUser)))
	}
	if inviterId != 0 {
		if config.QuotaForInvitee > 0 {
			_ = IncreaseUserQuota(user.Id, config.QuotaForInvitee)
			RecordLog(user.Id, LogTypeSystem, fmt.Sprintf("使用邀请码赠送 %s", common.LogQuota(config.QuotaForInvitee)))
		}
		if config.QuotaForInviter > 0 {
			_ = IncreaseUserQuota(inviterId, config.QuotaForInviter)
			RecordLog(inviterId, LogTypeSystem, fmt.Sprintf("邀请用户赠送 %s", common.LogQuota(config.QuotaForInviter)))
		}
	}
	return nil
}

func (user *User) Update(updatePassword bool) error {
	var err error
	if updatePassword {
		user.Password, err = common.Password2Hash(user.Password)
		if err != nil {
			return err
		}
	}
	if user.Status == common.UserStatusDisabled {
		blacklist.BanUser(user.Id)
	} else if user.Status == common.UserStatusEnabled {
		blacklist.UnbanUser(user.Id)
	}
	err = DB.Model(user).Updates(user).Error
	return err
}

func (user *User) Delete() error {
	if user.Id == 0 {
		return errors.New("id is required")
	}
	blacklist.BanUser(user.Id)
	user.Username = fmt.Sprintf("deleted_%s", helper.GetUUID())
	user.Status = common.UserStatusDeleted
	err := DB.Model(user).Updates(user).Error
	return err
}

// ValidateAndFill check password & user status
func (user *User) ValidateAndFill() (err error) {
	// When querying with struct, GORM will only query with non-zero fields,
	// that means if your field’s value is 0, '', false or other zero values,
	// it won’t be used to build query conditions
	password := user.Password
	if user.Username == "" || password == "" {
		return errors.New("username and password are required")
	}
	err = DB.Where("username = ?", user.Username).First(user).Error
	if err != nil {
		// we must make sure check username firstly
		// consider this case: a malicious user set his username as other's email
		err := DB.Where("email = ?", user.Username).First(user).Error
		if err != nil {
			return errors.New("invalid username or password, or account is suspended")
		}
	}
	okay := common.ValidatePasswordAndHash(password, user.Password)
	if !okay || user.Status != common.UserStatusEnabled {
		return errors.New("invalid username or password, or account is suspended")
	}
	return nil
}

func (user *User) FillUserById() error {
	if user.Id == 0 {
		return errors.New("id is required")
	}
	DB.Where(User{Id: user.Id}).First(user)
	return nil
}

func (user *User) FillUserByEmail() error {
	if user.Email == "" {
		return errors.New("email is required")
	}
	DB.Where(User{Email: user.Email}).First(user)
	return nil
}

func (user *User) FillUserByGitHubId() error {
	if user.GitHubId == "" {
		return errors.New("GitHub id is required")
	}
	DB.Where(User{GitHubId: user.GitHubId}).First(user)
	return nil
}

func (user *User) FillUserByGoogleId() error {
	if user.GoogleId == "" {
		return errors.New("Google id is required")
	}
	DB.Where(User{GoogleId: user.GoogleId}).First(user)
	return nil
}
func (user *User) FillUserByWeChatId() error {
	if user.WeChatId == "" {
		return errors.New("WeChat id is required")
	}
	DB.Where(User{WeChatId: user.WeChatId}).First(user)
	return nil
}

func (user *User) FillUserByUsername() error {
	if user.Username == "" {
		return errors.New("username is required")
	}
	DB.Where(User{Username: user.Username}).First(user)
	return nil
}

func IsEmailAlreadyTaken(email string) bool {
	return DB.Where("email = ?", email).Find(&User{}).RowsAffected == 1
}

func IsWeChatIdAlreadyTaken(wechatId string) bool {
	return DB.Where("wechat_id = ?", wechatId).Find(&User{}).RowsAffected == 1
}

func IsGitHubIdAlreadyTaken(githubId string) bool {
	return DB.Where("github_id = ?", githubId).Find(&User{}).RowsAffected == 1
}

func IsUsernameAlreadyTaken(username string) bool {
	return DB.Where("username = ?", username).Find(&User{}).RowsAffected == 1
}

func IsGoogleIdAlreadyTaken(GoogleId string) bool {
	return DB.Where("google_id = ?", GoogleId).Find(&User{}).RowsAffected == 1
}

func ResetUserPasswordByEmail(email string, password string) error {
	if email == "" || password == "" {
		return errors.New("email and password are required")
	}
	hashedPassword, err := common.Password2Hash(password)
	if err != nil {
		return err
	}
	err = DB.Model(&User{}).Where("email = ?", email).Update("password", hashedPassword).Error
	return err
}

func IsAdmin(userId int) bool {
	if userId == 0 {
		return false
	}
	var user User
	err := DB.Where("id = ?", userId).Select("role").Find(&user).Error
	if err != nil {
		logger.SysError("no such user " + err.Error())
		return false
	}
	return user.Role >= common.RoleAdminUser
}

func IsUserEnabled(userId int) (bool, error) {
	if userId == 0 {
		return false, errors.New("user id is empty")
	}
	var user User
	err := DB.Where("id = ?", userId).Select("status").Find(&user).Error
	if err != nil {
		return false, err
	}
	return user.Status == common.UserStatusEnabled, nil
}

func ValidateAccessToken(token string) (user *User) {
	if token == "" {
		return nil
	}
	token = strings.Replace(token, "Bearer ", "", 1)
	user = &User{}
	if DB.Where("access_token = ?", token).First(user).RowsAffected == 1 {
		return user
	}
	return nil
}

func GetUserQuota(id int) (quota int64, err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Select("quota").Find(&quota).Error
	return quota, err
}

func GetUserUsedQuota(id int) (quota int64, err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Select("used_quota").Find(&quota).Error
	return quota, err
}

func GetUserEmail(id int) (email string, err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Select("email").Find(&email).Error
	return email, err
}

func GetUserGroup(id int) (group string, err error) {
	groupCol := "`group`"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
	}

	err = DB.Model(&User{}).Where("id = ?", id).Select(groupCol).Find(&group).Error
	return group, err
}

func IncreaseUserQuota(id int, quota int64) (err error) {
	if quota < 0 {
		return errors.New("quota cannot be negative")
	}
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUserQuota, id, quota)
		return nil
	}
	return increaseUserQuota(id, quota)
}

func increaseUserQuota(id int, quota int64) (err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota + ?", quota)).Error
	return err
}

func DecreaseUserQuota(id int, quota int64) (err error) {
	if quota < 0 {
		return errors.New("quota cannot be negative")
	}
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUserQuota, id, -quota)
		return nil
	}
	return decreaseUserQuota(id, quota)
}

func decreaseUserQuota(id int, quota int64) (err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota - ?", quota)).Error
	return err
}

func GetRootUserEmail() (email string) {
	DB.Model(&User{}).Where("role = ?", common.RoleRootUser).Select("email").Find(&email)
	return email
}

func UpdateUserUsedQuotaAndRequestCount(id int, quota int64) {
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUsedQuota, id, quota)
		addNewRecord(BatchUpdateTypeRequestCount, id, 1)
		return
	}
	updateUserUsedQuotaAndRequestCount(id, quota, 1)
}

func updateUserUsedQuotaAndRequestCount(id int, quota int64, count int) {
	err := DB.Model(&User{}).Where("id = ?", id).Updates(
		map[string]interface{}{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"request_count": gorm.Expr("request_count + ?", count),
		},
	).Error
	if err != nil {
		logger.SysError("failed to update user used quota and request count: " + err.Error())
	}
}

func updateUserUsedQuota(id int, quota int64) {
	err := DB.Model(&User{}).Where("id = ?", id).Updates(
		map[string]interface{}{
			"used_quota": gorm.Expr("used_quota + ?", quota),
		},
	).Error
	if err != nil {
		logger.SysError("failed to update user used quota: " + err.Error())
	}
}

func updateUserRequestCount(id int, count int) {
	err := DB.Model(&User{}).Where("id = ?", id).Update("request_count", gorm.Expr("request_count + ?", count)).Error
	if err != nil {
		logger.SysError("failed to update user request count: " + err.Error())
	}
}

func GetUsernameById(id int) (username string) {
	DB.Model(&User{}).Where("id = ?", id).Select("username").Find(&username)
	return username
}

// CompensateVideoTaskQuota 补偿视频任务失败时的用户配额
// 单条 SQL 同时更新余额、已使用配额和请求次数
func CompensateVideoTaskQuota(userId int, quota int64) error {
	return DB.Model(&User{}).Where("id = ?", userId).Updates(
		map[string]interface{}{
			"quota":         gorm.Expr("quota + ?", quota),
			"used_quota":    gorm.Expr("used_quota - ?", quota),
			"request_count": gorm.Expr("request_count - 1"),
		},
	).Error
}
