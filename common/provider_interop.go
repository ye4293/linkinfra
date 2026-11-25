package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
)

// ProviderInteropRulesOption 是互通规则在系统设置里的 key。
const ProviderInteropRulesOption = "ProviderInteropRules"

const (
	providerInteropMaxRules       = 64
	providerInteropMaxPatterns    = 32
	providerInteropMaxProviders   = 32
	providerInteropMaxPatternSize = 256
	// 与渠道 provider、令牌路由里来源名的长度上限一致。
	providerInteropMaxNameSize = 128
)

// ProviderInteropRule 声明一组可以互相重试的 provider。
// 请求的模型匹配 Models 中任一正则、且首选渠道的 provider 在 Providers 里时，
// 本次请求的重试范围是 Providers；没有命中任何规则时，重试范围仍是首选渠道的 provider 一个。
//
// 规则只影响失败重试可以用哪些渠道，不影响首选。资源是否真的互通由管理员确认。
type ProviderInteropRule struct {
	// Models 是模型名正则，匹配请求里的模型名，任一匹配即可。
	Models []string `json:"models"`
	// Providers 是渠道上填写的 provider 值（小写），至少两个。
	Providers []string `json:"providers"`
}

type compiledProviderInteropRule struct {
	models    []*regexp.Regexp
	providers []string
}

// 当前生效的规则。整体替换，读取不加锁。
var providerInteropRules atomic.Pointer[[]compiledProviderInteropRule]

// ParseProviderInteropRules 解析并校验互通规则，返回规范化后的规则（provider 去空格、转小写、去重）。
// 空串、null 和 [] 都表示没有规则。每条规则只能有 models 和 providers 两个字段，
// 写错的字段会被拒绝而不是静默忽略。
func ParseProviderInteropRules(raw string) ([]ProviderInteropRule, error) {
	rules, _, err := parseProviderInteropRules(raw)
	return rules, err
}

func parseProviderInteropRules(raw string) ([]ProviderInteropRule, []compiledProviderInteropRule, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil, nil, nil
	}
	if trimmed[0] != '[' {
		return nil, nil, errors.New("provider interop rules must be a JSON array")
	}
	var parsed []ProviderInteropRule
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return nil, nil, fmt.Errorf("provider interop rules are invalid: %v", err)
	}
	if decoder.More() {
		return nil, nil, errors.New("provider interop rules must be a single JSON array")
	}
	if len(parsed) > providerInteropMaxRules {
		return nil, nil, fmt.Errorf("provider interop rules: at most %d rules are allowed", providerInteropMaxRules)
	}

	rules := make([]ProviderInteropRule, 0, len(parsed))
	compiled := make([]compiledProviderInteropRule, 0, len(parsed))
	for i, rule := range parsed {
		number := i + 1
		if len(rule.Models) == 0 {
			return nil, nil, fmt.Errorf("provider interop rule %d: \"models\" must contain at least one pattern", number)
		}
		if len(rule.Models) > providerInteropMaxPatterns {
			return nil, nil, fmt.Errorf("provider interop rule %d: at most %d model patterns are allowed", number, providerInteropMaxPatterns)
		}
		patterns := make([]string, 0, len(rule.Models))
		expressions := make([]*regexp.Regexp, 0, len(rule.Models))
		for _, pattern := range rule.Models {
			pattern = strings.TrimSpace(pattern)
			// 空的正则会匹配所有模型，按写错处理。
			if pattern == "" {
				return nil, nil, fmt.Errorf("provider interop rule %d: model patterns must not be empty", number)
			}
			if len(pattern) > providerInteropMaxPatternSize {
				return nil, nil, fmt.Errorf("provider interop rule %d: model patterns must not be longer than %d characters", number, providerInteropMaxPatternSize)
			}
			expression, err := regexp.Compile(pattern)
			if err != nil {
				return nil, nil, fmt.Errorf("provider interop rule %d: invalid model pattern %q: %v", number, pattern, err)
			}
			patterns = append(patterns, pattern)
			expressions = append(expressions, expression)
		}

		if len(rule.Providers) > providerInteropMaxProviders {
			return nil, nil, fmt.Errorf("provider interop rule %d: at most %d providers are allowed", number, providerInteropMaxProviders)
		}
		providers := make([]string, 0, len(rule.Providers))
		seen := make(map[string]bool, len(rule.Providers))
		for _, name := range rule.Providers {
			provider := strings.ToLower(strings.TrimSpace(name))
			if provider == "" {
				return nil, nil, fmt.Errorf("provider interop rule %d: provider names must not be empty", number)
			}
			if len(provider) > providerInteropMaxNameSize {
				return nil, nil, fmt.Errorf("provider interop rule %d: provider names must not be longer than %d characters", number, providerInteropMaxNameSize)
			}
			if !seen[provider] {
				seen[provider] = true
				providers = append(providers, provider)
			}
		}
		if len(providers) < 2 {
			return nil, nil, fmt.Errorf("provider interop rule %d: \"providers\" must list at least two different providers", number)
		}
		rules = append(rules, ProviderInteropRule{Models: patterns, Providers: providers})
		compiled = append(compiled, compiledProviderInteropRule{models: expressions, providers: providers})
	}
	return rules, compiled, nil
}

// UpdateProviderInteropRulesByJSONString 校验并启用互通规则。不合法时返回错误，当前生效的规则不变。
func UpdateProviderInteropRulesByJSONString(raw string) error {
	_, compiled, err := parseProviderInteropRules(raw)
	if err != nil {
		return err
	}
	providerInteropRules.Store(&compiled)
	return nil
}

// InteropProviders 返回请求这个模型时可以与 provider 互相重试的一组 provider（包含它自己）。
// 取第一条「模型匹配且 providers 包含 provider」的规则；没有这样的规则时返回 nil。
func InteropProviders(modelName, provider string) []string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	rules := providerInteropRules.Load()
	if provider == "" || rules == nil {
		return nil
	}
	for _, rule := range *rules {
		listed := false
		for _, candidate := range rule.providers {
			if candidate == provider {
				listed = true
				break
			}
		}
		if !listed {
			continue
		}
		for _, expression := range rule.models {
			if expression.MatchString(modelName) {
				return append([]string(nil), rule.providers...)
			}
		}
	}
	return nil
}
