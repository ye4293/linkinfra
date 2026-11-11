package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 计费标签是渠道上的可选分组，只用于用户折扣和消耗统计。
// 它不参与选渠、失败重试和 Responses 状态绑定（这些由 provider 决定），
// 所以打了标签的渠道仍然可以和其他渠道互相重试。

// UserChannelRatioTagPrefix 是 users.channel_ratios 中计费标签折扣的 key 前缀，
// 例如 {"48":0.9,"tag:vertex-claude":0.85}；纯数字 key 仍表示渠道类型。
const UserChannelRatioTagPrefix = "tag:"

const billingTagMaxLength = 64

// NormalizeBillingTag 忽略大小写和首尾空格；空值、超长或含非法字符时返回空串（视为未配置）。
// 只允许 ASCII 字母、数字和 . _ -，保证标签可以直接拼进 channel_ratios 的 key。
// 逐字节检查后再转小写，不用 strings.ToLower：它会把个别非 ASCII 字符（如开尔文符号 U+212A）
// 折叠成 ASCII 字母，让本该被拒绝的标签变成另一个合法标签。
func NormalizeBillingTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || len(tag) > billingTagMaxLength {
		return ""
	}
	normalized := make([]byte, len(tag))
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-':
		default:
			return ""
		}
		normalized[i] = c
	}
	return string(normalized)
}

// rawBillingTag 只取出 config 里的 billing_tag，不经过 LoadConfig。
// config 里别的字段类型不对时（例如 Vertex 的区域映射被存成了字符串）LoadConfig 整体报错，
// 而转发仍在用解析出的其余配置；标签的读取和校验都只看 billing_tag，不受其他字段影响。
func rawBillingTag(config string) (string, error) {
	if strings.TrimSpace(config) == "" {
		return "", nil
	}
	var probe struct {
		BillingTag string `json:"billing_tag"`
	}
	err := json.Unmarshal([]byte(config), &probe)
	return probe.BillingTag, err
}

// ChannelBillingTag 返回渠道 config.billing_tag 规范化后的值，未配置或非法时为空串。
func ChannelBillingTag(channel *Channel) string {
	if channel == nil {
		return ""
	}
	tag, err := rawBillingTag(channel.Config)
	if err != nil {
		return ""
	}
	return NormalizeBillingTag(tag)
}

// HasInvalidBillingTag 供保存渠道时校验：填了标签但规范化后为空（超长或含非法字符），
// 或者 billing_tag 不是字符串（这会让 LoadConfig 整体报错，连带 Provider 等配置也读不出来）。
func HasInvalidBillingTag(channel *Channel) bool {
	if channel == nil {
		return false
	}
	tag, err := rawBillingTag(channel.Config)
	if err != nil {
		// 探测用的结构体只有 billing_tag 一个字段：config 是 JSON 对象时，类型错误只可能来自它
		// （包括同一个 key 重复出现、其中一次不是字符串）。config 本身不是合法的 JSON 对象
		// 不属于标签校验的范围。
		var typeErr *json.UnmarshalTypeError
		return errors.As(err, &typeErr) && strings.HasPrefix(strings.TrimSpace(channel.Config), "{")
	}
	return strings.TrimSpace(tag) != "" && NormalizeBillingTag(tag) == ""
}

// UserChannelRatios 是用户的渠道折扣配置：按渠道类型，或按渠道的计费标签。
type UserChannelRatios struct {
	Types map[int]float64
	Tags  map[string]float64
}

// Resolve 是用户渠道折扣取值顺序的唯一定义：
// 渠道有标签且用户配置了该标签，用标签折扣；否则用渠道类型折扣；都没有则为 1.0。
// 标签折扣覆盖类型折扣，不叠乘。matchedTag 非空表示折扣来自该标签。
func (r UserChannelRatios) Resolve(channelType int, billingTag string) (ratio float64, matchedTag string) {
	if billingTag != "" {
		if v, ok := r.Tags[billingTag]; ok && v > 0 {
			return v, billingTag
		}
	}
	if channelType > 0 {
		if v, ok := r.Types[channelType]; ok && v > 0 {
			return v, ""
		}
	}
	return 1.0, ""
}

// ResolveUserChannelRatio 读取用户的渠道折扣配置并按 Resolve 取值，同步选渠和异步结算共用。
// 未登录、没有配置或读取失败时为 1.0，不会因为取不到数据阻塞计费。
func ResolveUserChannelRatio(userId int, channelType int, billingTag string) (ratio float64, matchedTag string) {
	ratios, err := CacheGetUserChannelRatios(userId)
	if err != nil {
		return 1.0, ""
	}
	return ratios.Resolve(channelType, billingTag)
}

// ratioKeyBillingTag 判断 channel_ratios 的一个 key 是不是计费标签折扣（tag:<标签>，前缀不分大小写）。
// isTag 为 true 而 tag 为空，表示这是一个标签项，但标签名不合法。
func ratioKeyBillingTag(key string) (tag string, isTag bool) {
	key = strings.TrimSpace(key)
	n := len(UserChannelRatioTagPrefix)
	if len(key) < n || !strings.EqualFold(key[:n], UserChannelRatioTagPrefix) {
		return "", false
	}
	return NormalizeBillingTag(key[n:]), true
}

// ValidateUserChannelRatios 供保存用户时校验 channel_ratios：空串（没有折扣），
// 或 key 为渠道类型 / tag:<标签>、值为数字的 JSON 对象。
// 保存后会在计费时静默失效的内容都在这里拒绝：
//   - 有一项的值不是数字：计费侧整份解析失败，所有折扣（包括原本正常的渠道类型折扣）一起失效；
//   - tag: 项的标签名不合法：永远不会命中；
//   - 既不是渠道类型也不是 tag: 项的 key（例如漏写了 tag: 前缀的标签名）：读取时被忽略。
//
// 非正数沿用既有约定：可以保存，读取时视为未设置。
func ValidateUserChannelRatios(raw string) error {
	if raw == "" {
		return nil
	}
	// JSON 的 null 解析后 map 仍是 nil，和空对象区分开：它不是折扣配置。
	var entries map[string]float64
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || entries == nil {
		return errors.New(`channel_ratios must be a JSON object that maps channel types or "tag:<billing tag>" keys to numbers`)
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if tag, isTag := ratioKeyBillingTag(key); isTag {
			if tag == "" {
				return fmt.Errorf("channel_ratios has an invalid billing tag key %q: billing tags may only contain letters, digits, '.', '_' and '-' (max %d characters)", key, billingTagMaxLength)
			}
			continue
		}
		if channelType, err := strconv.Atoi(key); err != nil || channelType <= 0 {
			return fmt.Errorf("channel_ratios has an unrecognized key %q: a key must be a channel type (a positive integer) or \"tag:<billing tag>\"", key)
		}
	}
	return nil
}

// parseUserChannelRatios 解析 channel_ratios 的原始内容，非正数和无法识别的 key 被忽略。
func parseUserChannelRatios(raw map[string]float64) UserChannelRatios {
	result := UserChannelRatios{Types: map[int]float64{}, Tags: map[string]float64{}}
	// 按 key 排序后处理：同一标签出现多种写法（大小写、空格）时结果固定，不让 map 遍历顺序影响计费。
	// 非正数的项先被忽略；剩下的写法里以规范写法（tag:<规范标签>）为准，没有规范写法时取排在最前的。
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := raw[k]
		if v <= 0 {
			continue
		}
		if tag, isTag := ratioKeyBillingTag(k); isTag {
			if tag == "" {
				continue
			}
			if _, exists := result.Tags[tag]; !exists || k == UserChannelRatioTagPrefix+tag {
				result.Tags[tag] = v
			}
			continue
		}
		// 渠道类型都是正整数；不大于 0 的 key 取值时用不到，也不必带进缓存和返回给用户的内容里。
		if channelType, err := strconv.Atoi(k); err == nil && channelType > 0 {
			result.Types[channelType] = v
		}
	}
	return result
}

// encode 序列化为与 users.channel_ratios 相同的格式（写缓存用），没有任何折扣时返回空串。
func (r UserChannelRatios) encode() string {
	if len(r.Types) == 0 && len(r.Tags) == 0 {
		return ""
	}
	raw := make(map[string]float64, len(r.Types)+len(r.Tags))
	for channelType, v := range r.Types {
		raw[strconv.Itoa(channelType)] = v
	}
	for tag, v := range r.Tags {
		raw[UserChannelRatioTagPrefix+tag] = v
	}
	bs, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(bs)
}

// StripBillingTagRatios 返回 channel_ratios 里可以给用户本人看的部分：只有按渠道类型的折扣。
// 标签名是管理端的内部分组，不向终端用户暴露。这里用计费的同一个解析器解析，再只输出渠道类型项，
// 所以返回的折扣与实际计费用到的一致，也不依赖在原文里辨认标签项；内容无法解析时返回空串。
func StripBillingTagRatios(raw string) string {
	if raw == "" {
		return ""
	}
	ratios, err := decodeChannelRatiosJSON(raw)
	if err != nil {
		return ""
	}
	ratios.Tags = nil
	return ratios.encode()
}

// BillingTagUsage 是一个计费标签及配置了它的渠道数（含已禁用的渠道）。
type BillingTagUsage struct {
	Tag          string `json:"tag"`
	ChannelCount int    `json:"channel_count"`
}

// ListChannelBillingTags 汇总所有渠道上配置的计费标签，按标签名排序。
func ListChannelBillingTags() ([]BillingTagUsage, error) {
	var channels []*Channel
	if err := DB.Select("id, config").Where("config <> ?", "").Find(&channels).Error; err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, channel := range channels {
		if tag := ChannelBillingTag(channel); tag != "" {
			counts[tag]++
		}
	}
	tags := make([]BillingTagUsage, 0, len(counts))
	for tag, count := range counts {
		tags = append(tags, BillingTagUsage{Tag: tag, ChannelCount: count})
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Tag < tags[j].Tag })
	return tags, nil
}
