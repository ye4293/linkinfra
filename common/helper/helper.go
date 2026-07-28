package helper

import (
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

func OpenBrowser(url string) {
	var err error

	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	}
	if err != nil {
		log.Println(err)
	}
}

func GetIp() (ip string) {
	ips, err := net.InterfaceAddrs()
	if err != nil {
		log.Println(err)
		return ip
	}

	for _, a := range ips {
		if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				ip = ipNet.IP.String()
				if strings.HasPrefix(ip, "10") {
					return
				}
				if strings.HasPrefix(ip, "172") {
					return
				}
				if strings.HasPrefix(ip, "192.168") {
					return
				}
				ip = ""
			}
		}
	}
	return
}

var sizeKB = 1024
var sizeMB = sizeKB * 1024
var sizeGB = sizeMB * 1024

func Bytes2Size(num int64) string {
	numStr := ""
	unit := "B"
	if num/int64(sizeGB) > 1 {
		numStr = fmt.Sprintf("%.2f", float64(num)/float64(sizeGB))
		unit = "GB"
	} else if num/int64(sizeMB) > 1 {
		numStr = fmt.Sprintf("%d", int(float64(num)/float64(sizeMB)))
		unit = "MB"
	} else if num/int64(sizeKB) > 1 {
		numStr = fmt.Sprintf("%d", int(float64(num)/float64(sizeKB)))
		unit = "KB"
	} else {
		numStr = fmt.Sprintf("%d", num)
	}
	return numStr + " " + unit
}

func Seconds2Time(num int) (time string) {
	if num/31104000 > 0 {
		time += strconv.Itoa(num/31104000) + "y "
		num %= 31104000
	}
	if num/2592000 > 0 {
		time += strconv.Itoa(num/2592000) + "mo "
		num %= 2592000
	}
	if num/86400 > 0 {
		time += strconv.Itoa(num/86400) + "d "
		num %= 86400
	}
	if num/3600 > 0 {
		time += strconv.Itoa(num/3600) + "h "
		num %= 3600
	}
	if num/60 > 0 {
		time += strconv.Itoa(num/60) + "m "
		num %= 60
	}
	time += strconv.Itoa(num) + "s"
	return
}

func Interface2String(inter interface{}) string {
	switch inter := inter.(type) {
	case string:
		return inter
	case int:
		return fmt.Sprintf("%d", inter)
	case float64:
		return fmt.Sprintf("%f", inter)
	}
	return "Not Implemented"
}

func UnescapeHTML(x string) interface{} {
	return template.HTML(x)
}

func IntMax(a int, b int) int {
	if a >= b {
		return a
	} else {
		return b
	}
}

func GetUUID() string {
	code := uuid.New().String()
	code = strings.Replace(code, "-", "", -1)
	return code
}

const keyChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
const keyNumbers = "0123456789"

// 这里刻意不调用 rand.Seed：
//
// 原实现在 init() 以及 GenerateKey / GetRandomString / GetRandomNumberString
// 里每次调用都执行 rand.Seed(time.Now().UnixNano())。Windows 的时钟精度很粗
// （约 0.5~15ms），同一个时钟 tick 内的多次调用会拿到相同的种子，于是返回
// 完全相同的随机串 —— 实测连续 5 次 GetRandomString(4) 全部返回 "CXo1"。
//
// 造成的实际问题：
//   - model/user.go 的 aff_code 有 uniqueIndex，两个用户在同一 tick 内注册
//     会拿到同一个邀请码，后者注册直接失败
//   - model/charge_order.go 的 appOrderId 是充值订单号，也是邀请返现的
//     幂等键（source_no），碰撞会让第二笔订单被误判为 webhook 重放
//   - controller/github.go 的 OAuth state 是 CSRF 防护参数，碰撞会削弱防护
//
// Go 1.20 起全局 rand 源已自动随机播种，rand.Seed 也随之废弃；调用它反而
// 会把全局源切出自动播种的快路径。删掉所有 Seed 调用后，每次取值都会推进
// PRNG 状态，连续调用自然得到不同结果。

func GenerateKey() string {
	key := make([]byte, 48)
	for i := 0; i < 16; i++ {
		key[i] = keyChars[rand.Intn(len(keyChars))]
	}
	uuid_ := GetUUID()
	for i := 0; i < 32; i++ {
		c := uuid_[i]
		if i%2 == 0 && c >= 'a' && c <= 'z' {
			c = c - 'a' + 'A'
		}
		key[i+16] = c
	}
	return string(key)
}

func GetRandomString(length int) string {
	key := make([]byte, length)
	for i := 0; i < length; i++ {
		key[i] = keyChars[rand.Intn(len(keyChars))]
	}
	return string(key)
}

func GetRandomNumberString(length int) string {
	key := make([]byte, length)
	for i := 0; i < length; i++ {
		key[i] = keyNumbers[rand.Intn(len(keyNumbers))]
	}
	return string(key)
}

func GetTimestamp() int64 {
	return time.Now().Unix()
}

func GetTimeString() string {
	now := time.Now()
	return fmt.Sprintf("%s%d", now.Format("20060102150405"), now.UnixNano()%1e9)
}

func GenRequestID() string {
	return GetTimeString() + GetRandomNumberString(8)
}

func Max(a int, b int) int {
	if a >= b {
		return a
	} else {
		return b
	}
}

func AssignOrDefault(value string, defaultValue string) string {
	if len(value) != 0 {
		return value
	}
	return defaultValue
}

func MessageWithRequestId(message string, id string) string {
	return fmt.Sprintf("%s (request id: %s)", message, id)
}

func String2Int(str string) int {
	num, err := strconv.Atoi(str)
	if err != nil {
		return 0
	}
	return num
}

func GetFormatTimeString() string {
	now := time.Now()
	return now.Format("20060102150405")
}
