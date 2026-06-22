package model

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
)

var Pubkey = "-----BEGIN PUBLIC KEY-----\nMIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQC3FT0Ym8b3myVxhQW7ESuuu6lo\ndGAsUJs4fq+Ey//jm27jQ7HHHDmP1YJO7XE7Jf/0DTEJgcw4EZhJFVwsk6d3+4fy\nBsn0tKeyGMiaE6cVkX0cy6Y85o8zgc/CwZKc0uw6d5siAo++xl2zl+RGMXCELQVE\nox7pp208zTvown577wIDAQAB\n-----END PUBLIC KEY-----"
var CryptHost = "https://api.cryptapi.io/"
var AesKey = "djde2322pv-qomx402jd3-pq2m49sj1l"

// User if you add sensitive fields, don't forget to clean them in setupLogin function.
// Otherwise, the sensitive information will be saved on local storage in plain text!
type CreateResponse struct {
	AddressIn   string `json:"address_in"`
	AddressOut  string `json:"address_out"`
	CallbackUrl string `json:"callback_url"`
	Priority    string `json:"priority"`
	Status      string `json:"status"` // admin, util
}
type QrcodeResponse struct {
	QrCode     string `json:"qr_code"`
	PaymentUri string `json:"payment_uri"`
	Status     string `json:"status"` // admin, util
}
type CryptCallbackResponse struct {
	UserId             int     `form:"user_id"        json:"user_id"`
	Uuid               string  `form:"uuid"           json:"uuid"`
	AddressIn          string  `form:"address_in"     json:"address_in"`
	AddressOut         string  `form:"address_out"    json:"address_out"`
	TxidIn             string  `form:"txid_in"        json:"txid_in"`
	TxidOut            string  `form:"txid_out"       json:"txid_out"`
	Confirmations      int64   `form:"confirmations"  json:"confirmations"`
	Value              int64   `form:"value"          json:"value"`
	ValueCoin          float64 `form:"value_coin"     json:"value_coin"`
	ValueForwarded     float64 `form:"value_forwarded" json:"value_forwarded"`
	ValueForwardedCoin float64 `form:"value_forwarded_coin" json:"value_forwarded_coin"`
	Fee                int64   `form:"fee"           json:"fee"`
	FeeCoin            float64 `form:"fee_coin"      json:"fee_coin"`
	Coin               string  `form:"coin"          json:"coin"`
	Price              float64 `form:"price"         json:"price"`
	Result             string  `form:"result"        json:"result"`
	Pending            int64   `form:"pending"       json:"pending"`
}
type PaymentLogsResponse struct {
	AddressIn           string      `json:"address_in"`
	AddressOut          string      `json:"address_out"`
	CallbackURL         string      `json:"callback_url"`
	Status              string      `json:"status"`
	NotifyPending       bool        `json:"notify_pending"`
	NotifyConfirmations int         `json:"notify_confirmations"`
	Priority            string      `json:"priority"`
	Callbacks           []Callbacks `json:"callbacks"`
}

type Logs struct {
	RequestURL     string `json:"request_url"`
	Response       string `json:"response"`
	ResponseStatus string `json:"response_status"`
	Timestamp      string `json:"timestamp"`
	NextTry        string `json:"next_try"`
	Success        bool   `json:"success"`
}

type Callbacks struct {
	TxidIn             string `json:"txid_in"`
	TxidOut            string `json:"txid_out"`
	Value              int64  `json:"value"`
	ValueCoin          string `json:"value_coin"`
	ValueForwarded     int64  `json:"value_forwarded"`
	ValueForwardedCoin string `json:"value_forwarded_coin"`
	Confirmations      int    `json:"confirmations"`
	LastUpdate         string `json:"last_update"`
	Result             string `json:"result"`
	FeePercent         string `json:"fee_percent"`
	Fee                int64  `json:"fee"`
	Logs               []Logs `json:"logs"`
}
type InfoResponse struct {
	Coin               string    `json:"coin"`
	MinimumTransaction float64   `json:"minimum_transaction"`
	MinimumFee         float64   `json:"minimum_fee"`
	FeePercent         string    `json:"fee_percent"`
	PricesUpdated      time.Time `json:"prices_updated"`
	Status             string    `json:"status"`
	Prices             struct {
		Usd string `json:"USD"`
		Eur string `json:"EUR"`
		Gbp string `json:"GBP"`
		Cad string `json:"CAD"`
		Jpy string `json:"JPY"`
		Aed string `json:"AED"`
		Dkk string `json:"DKK"`
		Brl string `json:"BRL"`
		Cny string `json:"CNY"`
		Hkd string `json:"HKD"`
		Inr string `json:"INR"`
		Mxn string `json:"MXN"`
		Ugx string `json:"UGX"`
		Pln string `json:"PLN"`
		Php string `json:"PHP"`
		Czk string `json:"CZK"`
		Huf string `json:"HUF"`
		Bgn string `json:"BGN"`
		Ron string `json:"RON"`
	} `json:"prices"`
}
type EstimateResponse struct {
	EstimatedCost    string `json:"estimated_cost"`
	EstimatedCostUsd string `json:"estimated_cost_usd"`
	Status           string `json:"status"`
}
type ConvertResponse struct {
	ValueCoin    string `json:"value_coin"`
	ExchangeRate string `json:"exchange_rate"`
	Status       string `json:"status"`
}

var CryptResponseResult = map[string]int{
	"pending":  1,
	"received": 2,
	"sent":     3,
	"done":     4,
}

func CryptGetRequest(requestURL string) ([]byte, error) {
	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}

	// 设置请求头部信息，如果需要的话
	// request.Header.Set("User-Agent", "自定义浏览器...")
	// request.Header.Set("Authorization", "Bearer token...")

	// 发送请求给服务端,实例化一个客户端
	client := &http.Client{}
	res, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// 服务端返回数据
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// 记录日志
	logger.SysLog(fmt.Sprintf("Request URL: %s; Response: %s", requestURL, string(b)))

	// 返回响应体的字节切片和错误
	return b, nil
}

func GetAddress(ticker string, userId int, params map[string]string) (*CreateResponse, error) {
	// 构建callback URL并进行编码
	baseCallbackURL := config.CryptCallbackUrl
	callbackParams := url.Values{}
	callbackParams.Add("user_id", strconv.Itoa(userId))
	// 正确地构建并编码回调URL
	encodedCallbackURL := baseCallbackURL + "?" + callbackParams.Encode()

	// 准备请求的其他参数
	requestParams := url.Values{}
	requestParams.Add("multi_token", "1")
	requestParams.Add("callback", encodedCallbackURL)
	// 确保从params中提取email和address，或者如果它们作为函数参数传入，直接使用
	requestParams.Add("address", config.AddressOut)
	requestParams.Add("email", "ye4293@gmail.com")

	// 构建最终的请求URL
	requestURL := CryptHost + ticker + "/create/?" + requestParams.Encode()

	// 发起请求
	response, err := CryptGetRequest(requestURL) // 假设CryptGetRequest函数现在只接受URL作为参数，并返回响应和错误
	if err != nil {
		return nil, err
	}
	var addressInfo CreateResponse
	err = json.Unmarshal(response, &addressInfo)
	if err != nil {
		return nil, err
	}
	return &addressInfo, nil
}

func GetQrcode(ticker string, userId int) (*QrcodeResponse, error) {
	// 获取地址信息
	addressInfo, err := GetAddress(ticker, userId, map[string]string{})
	if err != nil {
		return nil, err
	}
	if addressInfo.Status != "success" {
		return nil, errors.New("create address error")
	}

	// 构建请求URL，包括所需的查询参数
	url := fmt.Sprintf("%s%s/qrcode/?address=%s", CryptHost, ticker, url.QueryEscape(addressInfo.AddressIn))

	// 调用CryptGetRequest函数，传入完整的URL
	response, err := CryptGetRequest(url)
	if err != nil {
		return nil, err
	}

	// 解析响应体到QrcodeResponse结构体
	var qrcodeInfo QrcodeResponse
	err = json.Unmarshal(response, &qrcodeInfo)
	if err != nil {
		return nil, err
	}

	return &qrcodeInfo, nil
}

func GetInfo() {

}
func GetEstimate() {

}
func GetConvert() {
}
func GetLogs() {

}
func HandleCryptCallback(respons CryptCallbackResponse, username string) error {
	return CreateOrUpdateOrder(respons, username)
}
func VerifyCryptCallbackSignature(message, signature string) error {
	//
	decodeSignature, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("signature decode failed")
	}
	// 解析公钥PEM数据
	block, _ := pem.Decode([]byte(Pubkey))
	if block == nil {
		return errors.New("public key parse failed")
	}
	// 解析公钥
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}

	// 类型断言转换为*rsa.PublicKey
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return errors.New("public key is not RSA type")
	}

	//方法一：
	//创建一个基于SHA256算法的hash.Hash接口的对象
	hash := sha256.New()
	//输入数据
	hash.Write([]byte(message))
	//计算哈希值
	bytes := hash.Sum(nil)
	//将字符串编码为16进制格式,返回字符串
	//hashCode := hex.EncodeToString(bytes)
	// 使用crypto/rsa包的VerifyPKCS1v15方法验证签名
	err = rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, bytes, decodeSignature)
	if err != nil {
		return err
	}

	// 如果没有错误，表示签名验证成功
	return nil
}
func Encrypt(text string) (string, error) {
	key := []byte("djde2322pv-qomx402jd3-pq2m49sj1l") // 应该是32位长
	plaintext := []byte(text)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	ciphertext := make([]byte, aes.BlockSize+len(plaintext))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}
func Decrypt(encryptedText string) (string, error) {
	key := []byte(AesKey) // 应该是32位长
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedText)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < aes.BlockSize {
		return "", err
	}
	iv := ciphertext[:aes.BlockSize]
	ciphertext = ciphertext[aes.BlockSize:]

	stream := cipher.NewCFBDecrypter(block, iv)
	stream.XORKeyStream(ciphertext, ciphertext)

	return string(ciphertext), nil
}
