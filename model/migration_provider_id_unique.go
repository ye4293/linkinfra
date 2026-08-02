package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

// providerIdUniqueIndexes 需要保证唯一的 provider id 列。
//
// 这两列是 OAuth 登录的身份主键（见 controller/oauth_login.go）。同一个
// provider id 出现两行意味着「一个 GitHub 账号对应两个本站账号」，属于
// 数据损坏而不是一种合法状态。
//
// email 故意不在此列：邮箱注册流程（controller/user.go 的 Register）从来
// 不校验 email 唯一，加约束会让「同邮箱注册第二个账号」这个既有行为突然
// 开始失败。那条路径的危害已经在 ResetUserPasswordByEmail 里堵住了
// （命中多行时拒绝而不是全部改写）。
var providerIdUniqueIndexes = []struct {
	column string
	index  string
}{
	{column: "github_id", index: "idx_users_github_id_unique"},
	{column: "google_id", index: "idx_users_google_id_unique"},
}

// EnsureProviderIdUniqueIndexes 给 github_id / google_id 建**部分**唯一索引。
//
// 为什么必须是「部分」：邮箱注册的用户这两列都是空串，一个普通唯一索引会
// 让第二个邮箱注册用户直接插入失败。所以索引要带 WHERE <col> <> ” —— 只
// 约束真正绑定了 provider 的行。
//
// 为什么需要它：controller 层是「先查再建」（GetUserByGitHubId 查不到就
// Insert），两个并发请求可以同时查空、同时建号，各拿一份注册赠额。路由上的
// CriticalRateLimit 只是把窗口变窄，没有关掉它。DB 唯一索引是唯一能真正
// 关掉这个竞态的东西 —— 后写入的那个请求会失败，而不是静默产生两个账号。
//
// 方言差异：PostgreSQL 与 SQLite 都原生支持带 WHERE 的部分索引；MySQL
// 不支持（需要生成列绕，代价与收益不匹配），那里只记一条告警、继续靠
// 应用层的先查再建。
//
// 失败不阻止启动：索引建不上（通常是已有重复数据）时记 error 继续跑，
// 否则一次数据问题会让整个服务起不来。应用层的检查仍然在。
func EnsureProviderIdUniqueIndexes(db *gorm.DB) error {
	if common.UsingMySQL {
		logger.SysError("MySQL does not support partial indexes; " +
			"github_id / google_id uniqueness is enforced at the application layer only. " +
			"Concurrent OAuth registrations for the same provider id may still create duplicate accounts.")
		return nil
	}

	for _, target := range providerIdUniqueIndexes {
		dupes, err := findDuplicateProviderIds(db, target.column)
		if err != nil {
			return fmt.Errorf("failed to check duplicates on %s: %w", target.column, err)
		}
		if len(dupes) > 0 {
			// 有重复就建不了索引。列出来让人能去处理，而不是抛一条
			// 「duplicate key」了事 —— 那样得自己再去查是哪几行。
			logger.SysError(fmt.Sprintf(
				"cannot create unique index on users.%s: %d duplicated value(s) found (%v). "+
					"Merge or clear these accounts, then restart to create the index.",
				target.column, len(dupes), dupes))
			continue
		}

		// IF NOT EXISTS 让这个函数可以每次启动都跑（幂等）。
		sql := fmt.Sprintf(
			"CREATE UNIQUE INDEX IF NOT EXISTS %s ON users (%s) WHERE %s <> ''",
			target.index, target.column, target.column)
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("failed to create unique index %s: %w", target.index, err)
		}
	}
	return nil
}

// findDuplicateProviderIds 返回该列上出现多于一次的非空值。
func findDuplicateProviderIds(db *gorm.DB, column string) ([]string, error) {
	var dupes []string
	err := db.Model(&User{}).
		Select(column).
		Where(column+" <> ''").
		Group(column).
		Having("COUNT(*) > 1").
		Pluck(column, &dupes).Error
	if err != nil {
		return nil, err
	}
	return dupes, nil
}

// IsDuplicateProviderIdError 判断错误是否来自 provider id 的唯一索引冲突。
//
// 竞态下后写入的请求会拿到这个错误。调用方应当把它当成「这个 provider id
// 刚刚被另一个并发请求注册了」，而不是当成一次普通的 DB 故障 —— 前者重查
// 一次就能拿到那个刚建好的账号。
//
// 三种方言的错误文本不一样，必须都认：
//   - gorm 开启 TranslateError 时给 ErrDuplicatedKey
//   - PostgreSQL: `duplicate key value violates unique constraint "idx_users_github_id_unique"`
//     —— 报**索引**名
//   - SQLite: `UNIQUE constraint failed: users.github_id` —— 报**列**名，
//     完全不提索引名
//
// 只匹配索引名会让 SQLite 上的竞态恢复失效（实测踩到过），所以列名也要认；
// 但认列名时必须同时要求错误文本里有 unique/duplicate 之类的字样，否则任何
// 提到 github_id 的无关错误都会被误判成冲突。
func IsDuplicateProviderIdError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}

	msg := strings.ToLower(err.Error())

	// 先按索引名认（PG 走这条）
	for _, target := range providerIdUniqueIndexes {
		if strings.Contains(msg, strings.ToLower(target.index)) {
			return true
		}
	}

	// 再按「唯一性冲突 + 列名」认（SQLite / MySQL 走这条）
	looksLikeUniqueViolation := strings.Contains(msg, "unique") ||
		strings.Contains(msg, "duplicate")
	if !looksLikeUniqueViolation {
		return false
	}
	for _, target := range providerIdUniqueIndexes {
		if strings.Contains(msg, target.column) {
			return true
		}
	}
	return false
}
