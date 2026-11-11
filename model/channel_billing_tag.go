package model

import (
	"encoding/json"
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
// 只允许 a-z 0-9 . _ -，保证标签可以直接拼进 channel_ratios 的 key。
func NormalizeBillingTag(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" || len(tag) > billingTagMaxLength {
		return ""
	}
	for _, r := range tag {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !allowed {
			return ""
		}
	}
	return tag
}

// ChannelBillingTag 返回渠道 config.billing_tag 规范化后的值，未配置或非法时为空串。
func ChannelBillingTag(channel *Channel) string {
	if channel == nil {
		return ""
	}
	cfg, err := channel.LoadConfig()
	if err != nil {
		return ""
	}
	return NormalizeBillingTag(cfg.BillingTag)
}

// HasInvalidBillingTag 供保存渠道时校验：填了标签但规范化后为空（超长或含非法字符），
// 或者标签不是字符串。后者会让 LoadConfig 整体报错，连带 Provider 等配置也读不出来，
// 所以这里单独取出 billing_tag 判断，不依赖 LoadConfig 的结果。
func HasInvalidBillingTag(channel *Channel) bool {
	if channel == nil || channel.Config == "" {
		return false
	}
	var raw struct {
		BillingTag json.RawMessage `json:"billing_tag"`
	}
	if err := json.Unmarshal([]byte(channel.Config), &raw); err != nil || len(raw.BillingTag) == 0 {
		// config 不是合法的 JSON 对象，或没有 billing_tag：不是标签的问题。
		return false
	}
	var tag string
	if err := json.Unmarshal(raw.BillingTag, &tag); err != nil {
		return true
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

// parseUserChannelRatios 解析 channel_ratios 的原始内容，非正数和无法识别的 key 被忽略。
func parseUserChannelRatios(raw map[string]float64) UserChannelRatios {
	result := UserChannelRatios{Types: map[int]float64{}, Tags: map[string]float64{}}
	// 按 key 排序后处理：同一标签出现多种写法（大小写、空格）时结果固定，并以规范写法为准，
	// 不让 map 遍历顺序影响计费。
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
		if name, isTag := strings.CutPrefix(strings.ToLower(strings.TrimSpace(k)), UserChannelRatioTagPrefix); isTag {
			tag := NormalizeBillingTag(name)
			if tag == "" {
				continue
			}
			if _, exists := result.Tags[tag]; !exists || k == UserChannelRatioTagPrefix+tag {
				result.Tags[tag] = v
			}
			continue
		}
		if channelType, err := strconv.Atoi(k); err == nil {
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

// StripBillingTagRatios 去掉 channel_ratios 原始内容里的计费标签项，供返回给用户本人的接口使用：
// 标签名是管理端的内部分组，不向终端用户暴露。不含标签项时原样返回，渠道类型折扣不受影响。
func StripBillingTagRatios(raw string) string {
	if !strings.Contains(strings.ToLower(raw), UserChannelRatioTagPrefix) {
		return raw
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		// 无法解析就无法只去掉标签项，整体不返回。
		return ""
	}
	for key := range entries {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), UserChannelRatioTagPrefix) {
			delete(entries, key)
		}
	}
	if len(entries) == 0 {
		return ""
	}
	bs, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(bs)
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
