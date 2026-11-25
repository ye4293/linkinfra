package model

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// 令牌的来源路由：用户在 API key 上决定请求可以使用哪些上游来源、先后顺序，
// 以及永不使用的来源。来源名与模型广场的 source_key 相同（见 ChannelSource）。
//
// 没有设置路由（空值）时，选渠和重试与引入这个功能之前完全一致。
const providerRoutingMaxEntries = 32

// ErrTokenProviderRoutingNoChannel 表示分组和模型下有可用渠道，但都被令牌的来源路由排除。
// 这种情况不换到路由之外的来源，调用方据此返回说明原因的错误。
var ErrTokenProviderRoutingNoChannel = errors.New("no channel is available within the API key's provider routing")

// TokenProviderRouting 存在 tokens.provider_routing 的 JSON 文本里：
//
//	{"order":["vertex","anthropic"],"only":true,"ignore":["aws"]}
type TokenProviderRouting struct {
	// Order 是选中的来源，顺序即使用的先后。
	Order []string `json:"order"`
	// Only 对应界面上的 "Only Selected Providers"：为 true 时只用 Order 里的来源，
	// 为 false 时其余来源排在最后作为后备。
	Only bool `json:"only"`
	// Ignore 是永不使用的来源，优先级最高：首选、失败重试、后备层、亲和命中
	// 和 count_tokens 都不会选中它们。
	Ignore []string `json:"ignore"`
}

// IsEmpty 表示没有设置路由。
func (r TokenProviderRouting) IsEmpty() bool {
	return len(r.Order) == 0 && !r.Only && len(r.Ignore) == 0
}

// Equal 判断两份路由的内容是否相同（空列表和 nil 视为相同）。用于比较规范化后的路由。
func (r TokenProviderRouting) Equal(other TokenProviderRouting) bool {
	return r.Only == other.Only && slices.Equal(r.Order, other.Order) && slices.Equal(r.Ignore, other.Ignore)
}

// Allows 判断来源是否在允许范围内（ignore 优先，其次 only）。不涉及先后顺序。
func (r TokenProviderRouting) Allows(source string) bool {
	for _, ignored := range r.Ignore {
		if ignored == source {
			return false
		}
	}
	if !r.Only {
		return true
	}
	for _, selected := range r.Order {
		if selected == source {
			return true
		}
	}
	return false
}

// AllowsChannel 判断渠道的来源是否在允许范围内。没有设置路由时不读取渠道配置。
func (r TokenProviderRouting) AllowsChannel(channel *Channel) bool {
	if r.IsEmpty() {
		return true
	}
	return r.Allows(ChannelSource(channel))
}

// layerCount 返回选渠时依次尝试的层数：Order 里每个来源一层；
// Only 为 false 时其余来源合并成最后一层。没有 Order 时只有一层。
func (r TokenProviderRouting) layerCount() int {
	if len(r.Order) == 0 || !r.Only {
		return len(r.Order) + 1
	}
	return len(r.Order)
}

// layerOf 返回来源所在的层，不允许使用时为 -1。前面的层没有可选渠道才轮到后面的层。
func (r TokenProviderRouting) layerOf(source string) int {
	if !r.Allows(source) {
		return -1
	}
	for i, selected := range r.Order {
		if selected == source {
			return i
		}
	}
	// 不在 Order 里又被允许：没有 Order 时是唯一的一层，否则是 Only 为 false 时的后备层。
	return len(r.Order)
}

// String 用于日志和报错信息。
func (r TokenProviderRouting) String() string {
	return fmt.Sprintf("order=%v only=%t ignore=%v", r.Order, r.Only, r.Ignore)
}

// tokenProviderRoutingJSON 是 TokenProviderRouting 的 JSON 形式，不带方法，编解码时用它避免递归。
type tokenProviderRoutingJSON struct {
	Order  []string `json:"order"`
	Only   bool     `json:"only"`
	Ignore []string `json:"ignore"`
}

// routing 转成内存里的形式：空列表统一为 nil，同样的设置只有一种表示。
func (in tokenProviderRoutingJSON) routing() TokenProviderRouting {
	routing := TokenProviderRouting{Only: in.Only}
	if len(in.Order) > 0 {
		routing.Order = in.Order
	}
	if len(in.Ignore) > 0 {
		routing.Ignore = in.Ignore
	}
	return routing
}

// MarshalJSON 固定输出三个字段，列表为空时输出 []，接口的调用方不需要区分 null。
func (r TokenProviderRouting) MarshalJSON() ([]byte, error) {
	out := tokenProviderRoutingJSON{Order: r.Order, Only: r.Only, Ignore: r.Ignore}
	if out.Order == nil {
		out.Order = []string{}
	}
	if out.Ignore == nil {
		out.Ignore = []string{}
	}
	return json.Marshal(out)
}

// UnmarshalJSON 用于读取库里和令牌缓存里的内容：不拒绝未知字段，回滚到旧版本后仍能读出新版本写入的内容。
// 请求里提交的内容走 ParseTokenProviderRouting，那里会拒绝未知字段并校验。
func (r *TokenProviderRouting) UnmarshalJSON(data []byte) error {
	var in tokenProviderRoutingJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*r = in.routing()
	return nil
}

// Value 实现 driver.Valuer：没有设置路由时写 NULL。
func (r TokenProviderRouting) Value() (driver.Value, error) {
	if r.IsEmpty() {
		return nil, nil
	}
	bs, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return string(bs), nil
}

// Scan 实现 sql.Scanner，兼容 NULL 和不同驱动返回的 []byte / string。
// 内容不是合法的 JSON 时报错，不把写坏的路由当成没有设置。
func (r *TokenProviderRouting) Scan(value interface{}) error {
	var raw []byte
	switch v := value.(type) {
	case nil:
		*r = TokenProviderRouting{}
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("cannot scan %T into TokenProviderRouting", value)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		*r = TokenProviderRouting{}
		return nil
	}
	var routing TokenProviderRouting
	if err := json.Unmarshal(raw, &routing); err != nil {
		return fmt.Errorf("invalid provider_routing: %w", err)
	}
	*r = routing
	return nil
}

// normalizeProviderSourceList 规范化一个来源列表：去掉首尾空格并转小写，与 ChannelSource 的结果可以直接比较。
func normalizeProviderSourceList(field string, names []string) ([]string, error) {
	if len(names) > providerRoutingMaxEntries {
		return nil, fmt.Errorf("provider routing %q has more than %d providers", field, providerRoutingMaxEntries)
	}
	normalized := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		source := strings.ToLower(strings.TrimSpace(name))
		if source == "" {
			return nil, fmt.Errorf("provider routing %q contains an empty provider name", field)
		}
		if len(source) > providerNameMaxLength {
			return nil, fmt.Errorf("provider routing %q contains a provider name longer than %d characters", field, providerNameMaxLength)
		}
		if seen[source] {
			return nil, fmt.Errorf("provider routing %q lists the provider %q more than once", field, source)
		}
		seen[source] = true
		normalized = append(normalized, source)
	}
	return normalized, nil
}

// Normalize 返回写库用的规范形式，并校验保存时能发现的错误：
// 来源名不能为空或超长、每个列表最多 32 个且不重复、order 和 ignore 不能出现同一个来源、
// only 为 true 时 order 不能为空。不校验来源当前是否存在：来源会随渠道启停变化。
func (r TokenProviderRouting) Normalize() (TokenProviderRouting, error) {
	order, err := normalizeProviderSourceList("order", r.Order)
	if err != nil {
		return TokenProviderRouting{}, err
	}
	ignore, err := normalizeProviderSourceList("ignore", r.Ignore)
	if err != nil {
		return TokenProviderRouting{}, err
	}
	if r.Only && len(order) == 0 {
		return TokenProviderRouting{}, errors.New(`provider routing "only" requires at least one provider in "order"`)
	}
	selected := make(map[string]bool, len(order))
	for _, source := range order {
		selected[source] = true
	}
	for _, source := range ignore {
		if selected[source] {
			return TokenProviderRouting{}, fmt.Errorf(`provider routing lists the provider %q in both "order" and "ignore"`, source)
		}
	}
	// ignore 的顺序没有含义，排序后写库，同样的设置只有一种写法。
	sort.Strings(ignore)
	normalized := TokenProviderRouting{Only: r.Only}
	if len(order) > 0 {
		normalized.Order = order
	}
	if len(ignore) > 0 {
		normalized.Ignore = ignore
	}
	return normalized, nil
}

// ParseTokenProviderRouting 解析并校验请求里提交的 provider_routing。
// 必须是只含 order / only / ignore 的 JSON 对象：未知字段会被拒绝，拼错的设置不会被静默忽略。
// 空对象表示清空路由。调用方负责处理「没有提交」（字段缺失或为 null）的情况。
func ParseTokenProviderRouting(raw []byte) (TokenProviderRouting, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return TokenProviderRouting{}, errors.New("provider routing must be a JSON object")
	}
	var in tokenProviderRoutingJSON
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return TokenProviderRouting{}, fmt.Errorf("provider routing is invalid: %v", err)
	}
	if decoder.More() {
		return TokenProviderRouting{}, errors.New("provider routing must be a single JSON object")
	}
	return in.routing().Normalize()
}

type tokenProviderRoutingContextKey struct{}

// WithTokenProviderRouting 把令牌的来源路由带进请求上下文，选渠函数从这里读取。
func WithTokenProviderRouting(ctx context.Context, routing TokenProviderRouting) context.Context {
	return context.WithValue(ctx, tokenProviderRoutingContextKey{}, routing)
}

// TokenProviderRoutingFrom 返回本次请求适用的令牌路由；没有时为零值。
func TokenProviderRoutingFrom(ctx context.Context) TokenProviderRouting {
	routing, _ := ctx.Value(tokenProviderRoutingContextKey{}).(TokenProviderRouting)
	return routing
}
