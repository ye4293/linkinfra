package model

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/env"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var DB *gorm.DB
var LOG_DB *gorm.DB

func CreateRootAccountIfNeed() error {
	var user User
	//if user.Status != util.UserStatusEnabled {
	if err := DB.First(&user).Error; err != nil {
		logger.SysLog("no user exists, creating a root user for you: username is root, password is 12345678")
		hashedPassword, err := common.Password2Hash("12345678")
		if err != nil {
			return err
		}
		rootUser := User{
			Username:    "root",
			Password:    hashedPassword,
			Role:        common.RoleRootUser,
			Status:      common.UserStatusEnabled,
			DisplayName: "Root User",
			AccessToken: helper.GetUUID(),
			Quota:       500000000000000,
		}
		DB.Create(&rootUser)
		if config.InitialRootToken != "" {
			logger.SysLog("creating initial root token as requested")
			token := Token{
				Id:             1,
				UserId:         rootUser.Id,
				Key:            config.InitialRootToken,
				Status:         common.TokenStatusEnabled,
				Name:           "Initial Root Token",
				CreatedTime:    helper.GetTimestamp(),
				AccessedTime:   helper.GetTimestamp(),
				ExpiredTime:    -1,
				RemainQuota:    500000000000000,
				UnlimitedQuota: true,
			}
			DB.Create(&token)
		}
	}
	return nil
}

func chooseDB(envName string) (*gorm.DB, error) {
	if os.Getenv(envName) != "" {
		dsn := os.Getenv(envName)
		if strings.HasPrefix(dsn, "postgres://") {
			// Use PostgreSQL
			logger.SysLog("using PostgreSQL as database")
			common.UsingPostgreSQL = true
			return gorm.Open(postgres.New(postgres.Config{
				DSN:                  dsn,
				PreferSimpleProtocol: true, // disables implicit prepared statement usage
			}), &gorm.Config{
				PrepareStmt: true, // precompile SQL
			})
		}
		// Use MySQL
		logger.SysLog("using MySQL as database")
		common.UsingMySQL = true
		return gorm.Open(mysql.Open(dsn), &gorm.Config{
			PrepareStmt: true, // precompile SQL
		})
	}
	// Use SQLite
	logger.SysLog("SQL_DSN not set, using SQLite as database")
	common.UsingSQLite = true
	config := fmt.Sprintf("?_busy_timeout=%d&_journal_mode=WAL", common.SQLiteBusyTimeout)
	return gorm.Open(sqlite.Open(common.SQLitePath+config), &gorm.Config{
		PrepareStmt: true, // precompile SQL
	})
}

func InitDB(envName string) (db *gorm.DB, err error) {
	db, err = chooseDB(envName)
	if err == nil {
		if config.DebugSQLEnabled {
			db = db.Debug()
		}
		sqlDB, err := db.DB()
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxIdleConns(env.Int("SQL_MAX_IDLE_CONNS", 100))
		maxOpenConns := env.Int("SQL_MAX_OPEN_CONNS", 1000)
		if common.UsingSQLite && maxOpenConns > 1 {
			maxOpenConns = 1
		}
		sqlDB.SetMaxOpenConns(maxOpenConns)
		sqlDB.SetConnMaxLifetime(time.Second * time.Duration(env.Int("SQL_MAX_LIFETIME", 60)))

		if !config.IsMasterNode {
			return db, err
		}
		if common.UsingMySQL {
			_, _ = sqlDB.Exec("DROP INDEX idx_channels_key ON channels;") // TODO: delete this line when most users have upgraded
		}
		logger.SysLog("database migration started")
		err = db.AutoMigrate(&Channel{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Token{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&User{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Option{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Redemption{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Ability{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Log{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Order{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&File{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Bill{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Flux{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Video{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&Image{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&TopUp{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&GroupConfig{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&ModelMetrics{})
		if err != nil {
			return nil, err
		}
		err = db.AutoMigrate(&AffCommissionRecord{})
		if err != nil {
			return nil, err
		}
		logger.SysLog("database migrated")
		if err := InitGroupConfigs(db); err != nil {
			logger.SysError("failed to init group configs: " + err.Error())
		}
		// 回填失败只记 log 不阻止启动：等级判定会偏保守（只升不降保住现状），
		// 管理员可修好问题后删除 options 表里的 MigratedTopupQuotaV1 再重启。
		if err := BackfillTopupQuota(db); err != nil {
			logger.SysError("failed to backfill topup_quota: " + err.Error())
		}
		return db, err
	} else {
		logger.FatalLog(err)
	}
	return db, err
}

func closeDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	err = sqlDB.Close()
	return err
}

func CloseDB() error {
	if LOG_DB != DB {
		err := closeDB(LOG_DB)
		if err != nil {
			return err
		}
	}
	return closeDB(DB)
}
