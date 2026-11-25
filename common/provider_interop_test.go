package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 互通规则的解析和校验：规范化 provider，拒绝写错的规则。
func TestParseProviderInteropRules(t *testing.T) {
	for _, empty := range []string{"", "  ", "null", "[]", " [ ] "} {
		rules, err := ParseProviderInteropRules(empty)
		require.NoError(t, err, empty)
		require.Empty(t, rules, empty)
	}

	rules, err := ParseProviderInteropRules(` [
		{"models":[" ^claude- "],"providers":[" Anthropic ","VERTEX","aws","vertex"]},
		{"models":["^gpt-5","^o\\d"],"providers":["openai","azure-east"]}
	] `)
	require.NoError(t, err)
	require.Equal(t, []ProviderInteropRule{
		{Models: []string{"^claude-"}, Providers: []string{"anthropic", "vertex", "aws"}},
		{Models: []string{"^gpt-5", `^o\d`}, Providers: []string{"openai", "azure-east"}},
	}, rules, "provider 去空格、转小写、去重，顺序不变")

	tooManyRules := "[" + strings.TrimSuffix(strings.Repeat(`{"models":["x"],"providers":["a","b"]},`, 65), ",") + "]"
	tooManyPatterns := `[{"models":[` + strings.TrimSuffix(strings.Repeat(`"x",`, 33), ",") + `],"providers":["a","b"]}]`
	manyProviders := make([]string, 33)
	for i := range manyProviders {
		manyProviders[i] = `"p` + strings.Repeat("x", i) + `"`
	}
	for name, tc := range map[string]struct{ raw, message string }{
		"不是数组":             {`{"models":["^claude-"],"providers":["a","b"]}`, "must be a JSON array"},
		"字符串":              {`"^claude-"`, "must be a JSON array"},
		"JSON 不完整":         {`[{"models":["^claude-"]`, "are invalid"},
		"数组后面还有内容":         {`[] []`, "single JSON array"},
		"拼错的字段":            {`[{"model":["^claude-"],"providers":["a","b"]}]`, "are invalid"},
		"多余的字段":            {`[{"models":["^claude-"],"providers":["a","b"],"enabled":true}]`, "are invalid"},
		"没有 models":        {`[{"providers":["a","b"]}]`, `rule 1: "models" must contain at least one pattern`},
		"models 为空":        {`[{"models":[],"providers":["a","b"]}]`, `rule 1: "models" must contain at least one pattern`},
		"空的正则":             {`[{"models":["^claude-"],"providers":["a","b"]},{"models":["  "],"providers":["a","b"]}]`, "rule 2: model patterns must not be empty"},
		"正则不能编译":           {`[{"models":["claude-("],"providers":["a","b"]}]`, `rule 1: invalid model pattern "claude-("`},
		"正则超长":             {`[{"models":["` + strings.Repeat("a", 257) + `"],"providers":["a","b"]}]`, "must not be longer than 256"},
		"正则太多":             {tooManyPatterns, "at most 32 model patterns"},
		"规则太多":             {tooManyRules, "at most 64 rules"},
		"只有一个 provider":    {`[{"models":["^claude-"],"providers":["anthropic"]}]`, "at least two different providers"},
		"没有 providers":     {`[{"models":["^claude-"]}]`, "at least two different providers"},
		"去重后只剩一个 provider": {`[{"models":["^claude-"],"providers":["Vertex"," vertex "]}]`, "at least two different providers"},
		"空的 provider":      {`[{"models":["^claude-"],"providers":["anthropic"," "]}]`, "provider names must not be empty"},
		"provider 超长":      {`[{"models":["^claude-"],"providers":["anthropic","` + strings.Repeat("a", 129) + `"]}]`, "must not be longer than 128"},
		"provider 太多":      {`[{"models":["^claude-"],"providers":[` + strings.Join(manyProviders, ",") + `]}]`, "at most 32 providers"},
		"provider 不是字符串":   {`[{"models":["^claude-"],"providers":["anthropic",1]}]`, "are invalid"},
		"规则不是对象":           {`["^claude-"]`, "are invalid"},
	} {
		_, err := ParseProviderInteropRules(tc.raw)
		require.ErrorContains(t, err, tc.message, name)
	}

	// 上限以内可以保存。
	_, err = ParseProviderInteropRules(`[{"models":["` + strings.Repeat("a", 256) + `"],"providers":["a","` + strings.Repeat("b", 128) + `"]}]`)
	require.NoError(t, err)
}

// 重试范围的取值：第一条「模型匹配且 providers 包含首选渠道的 provider」的规则。
func TestInteropProviders(t *testing.T) {
	t.Cleanup(func() { require.NoError(t, UpdateProviderInteropRulesByJSONString("")) })
	require.Nil(t, InteropProviders("claude-opus", "anthropic"), "没有规则")

	require.NoError(t, UpdateProviderInteropRulesByJSONString(`[
		{"models":["^claude-"],"providers":["anthropic","vertex","aws"]},
		{"models":["^claude-","^gpt-"],"providers":["openrouter","vertex"]},
		{"models":["-thinking$","^o\\d"],"providers":["openai","azure-east"]}
	]`))

	require.Equal(t, []string{"anthropic", "vertex", "aws"}, InteropProviders("claude-opus-5", "anthropic"))
	require.Equal(t, []string{"anthropic", "vertex", "aws"}, InteropProviders("claude-opus-5", " AWS "), "provider 忽略大小写和首尾空格")
	require.Equal(t, []string{"anthropic", "vertex", "aws"}, InteropProviders("claude-opus-5", "vertex"), "两条规则都包含 vertex 时取第一条匹配的")
	require.Equal(t, []string{"openrouter", "vertex"}, InteropProviders("claude-opus-5", "openrouter"))
	require.Equal(t, []string{"openrouter", "vertex"}, InteropProviders("gpt-5", "vertex"), "第一条规则的模型不匹配，继续看后面的规则")
	require.Equal(t, []string{"openai", "azure-east"}, InteropProviders("gpt-5-thinking", "azure-east"), "任一正则匹配即可")
	require.Equal(t, []string{"openai", "azure-east"}, InteropProviders("o3", "openai"))

	require.Nil(t, InteropProviders("gemini-3-pro", "vertex"), "模型不匹配")
	require.Nil(t, InteropProviders("claude-opus-5", "azure"), "provider 不在任何规则里")
	require.Nil(t, InteropProviders("gpt-5", "anthropic"), "包含这个 provider 的规则模型不匹配")
	require.Nil(t, InteropProviders("claude-opus-5", ""), "没有填 provider 的渠道不参与互通")
	require.Nil(t, InteropProviders("Claude-opus-5", "anthropic"), "模型名区分大小写")

	// 返回的是副本，调用方修改不影响规则。
	scope := InteropProviders("claude-opus-5", "anthropic")
	scope[0] = "changed"
	require.Equal(t, []string{"anthropic", "vertex", "aws"}, InteropProviders("claude-opus-5", "anthropic"))

	// 不合法的更新被拒绝，当前规则保持不变。
	require.Error(t, UpdateProviderInteropRulesByJSONString(`[{"models":["("],"providers":["a","b"]}]`))
	require.Equal(t, []string{"anthropic", "vertex", "aws"}, InteropProviders("claude-opus-5", "anthropic"))

	// 清空。
	require.NoError(t, UpdateProviderInteropRulesByJSONString("[]"))
	require.Nil(t, InteropProviders("claude-opus-5", "anthropic"))
}
