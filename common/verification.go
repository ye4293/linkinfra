package common

import (
	cryptorand "crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/songquanpeng/one-api/common/logger"
)

type verificationValue struct {
	code string
	time time.Time
}

const (
	EmailVerificationPurpose = "v"
	PasswordResetPurpose     = "r"
)

var verificationMutex sync.Mutex
var verificationMap map[string]verificationValue
var verificationMapMaxSize = 10
var VerificationValidMinutes = 10

func GenerateVerificationCode(length int) string {
	code := uuid.New().String()
	code = strings.Replace(code, "-", "", -1)
	if length == 0 {
		return code
	}
	return code[:length]
}

// GeneratePassword 生成「忘记密码」流程用的新密码。
//
// 用 crypto/rand 而非 math/rand：这个值会直接写库并邮件发给用户，是凭据。
// 原实现是 rand.NewSource(time.Now().UnixNano()) + math/rand —— Windows
// 时钟精度约 0.5~15ms，同一 tick 内的两次重置会拿到相同种子、生成完全
// 相同的密码（实测第 2 次调用就撞），攻击者只要知道大致时间就能大幅缩小
// 猜测空间。
//
// 取模会引入轻微的模偏置（62 不整除 256），对 8 位密码的强度影响可忽略；
// 若日后要求更严格，可改用 rejection sampling。
func GeneratePassword() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const passwordLength = 8

	buf := make([]byte, passwordLength)
	if _, err := cryptorand.Read(buf); err != nil {
		// crypto/rand 在正常系统上不会失败；真失败了也不能退回可预测的
		// math/rand 生成凭据，直接返回空串让调用方的后续流程报错。
		logger.SysError("failed to read crypto/rand for password generation: " + err.Error())
		return ""
	}

	password := make([]byte, passwordLength)
	for i, b := range buf {
		password[i] = letters[int(b)%len(letters)]
	}
	return string(password)
}

func RegisterVerificationCodeWithKey(key string, code string, purpose string) {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	verificationMap[purpose+key] = verificationValue{
		code: code,
		time: time.Now(),
	}
	if len(verificationMap) > verificationMapMaxSize {
		removeExpiredPairs()
	}
}

func VerifyCodeWithKey(key string, code string, purpose string) bool {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	value, okay := verificationMap[purpose+key]
	now := time.Now()
	if !okay || int(now.Sub(value.time).Seconds()) >= VerificationValidMinutes*60 {
		return false
	}
	return code == value.code
}

func DeleteKey(key string, purpose string) {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	delete(verificationMap, purpose+key)
}

// no lock inside, so the caller must lock the verificationMap before calling!
func removeExpiredPairs() {
	now := time.Now()
	for key := range verificationMap {
		if int(now.Sub(verificationMap[key].time).Seconds()) >= VerificationValidMinutes*60 {
			delete(verificationMap, key)
		}
	}
}

func init() {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	verificationMap = make(map[string]verificationValue)
}
