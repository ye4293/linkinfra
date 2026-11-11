package model

import (
	"strings"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBillingTag(t *testing.T) {
	for input, want := range map[string]string{
		" Vertex-Claude ":       "vertex-claude",
		"vertex.gemini_2":       "vertex.gemini_2",
		"":                      "",
		"   ":                   "",
		"vertex claude":         "",
		"tag:vertex":            "",
		"渠道":                    "",
		strings.Repeat("a", 64): strings.Repeat("a", 64),
		strings.Repeat("a", 65): "",
		// 非 ASCII 字符一律拒绝，不会被大小写折叠成另一个合法标签（U+212A 开尔文符号的小写是 k）。
		// 用码点构造，免得源码里出现和 ASCII 字母长得一样的字符。
		"vertex-" + string(rune(0x212A)) + "laude": "",
		string(rune(0x0130)) + "stanbul":           "",
	} {
		require.Equal(t, want, NormalizeBillingTag(input), "input %q", input)
	}
}

func TestChannelBillingTag(t *testing.T) {
	require.Equal(t, "vertex-claude", ChannelBillingTag(&Channel{Config: `{"provider":"azure","billing_tag":" Vertex-Claude "}`}))
	require.Equal(t, "", ChannelBillingTag(&Channel{Config: `{"provider":"azure"}`}))
	require.Equal(t, "", ChannelBillingTag(&Channel{Config: `{"billing_tag":"bad tag"}`}))
	require.Equal(t, "", ChannelBillingTag(&Channel{Config: `not json`}))
	require.Equal(t, "", ChannelBillingTag(&Channel{}))
	require.Equal(t, "", ChannelBillingTag(nil))
	// config 里别的字段类型不对时（Vertex 区域映射被存成了字符串）LoadConfig 报错，
	// 转发仍在用解析出的其余配置，标签也要照常读得出来。
	brokenRegion := &Channel{Config: `{"billing_tag":"vertex-claude","region":"global","vertex_model_region":"{bad json"}`}
	_, err := brokenRegion.LoadConfig()
	require.Error(t, err)
	require.Equal(t, "vertex-claude", ChannelBillingTag(brokenRegion))
	// 标签与 provider 互不影响：只打标签不会产生重试边界。
	require.Equal(t, "", ChannelProvider(&Channel{Config: `{"billing_tag":"vertex-claude"}`}))

	require.True(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":"bad tag"}`}))
	require.True(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":"` + strings.Repeat("a", 65) + `"}`}))
	require.False(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":" Vertex-Claude "}`}))
	require.False(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":"  "}`}))
	require.False(t, HasInvalidBillingTag(&Channel{}))
	require.False(t, HasInvalidBillingTag(nil))
	// 标签不是字符串时整个 config 解析报错（Provider 等配置也读不出来），保存时同样拒绝。
	require.True(t, HasInvalidBillingTag(&Channel{Config: `{"provider":"azure","billing_tag":123}`}))
	require.True(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":["vertex-claude"]}`}))
	require.False(t, HasInvalidBillingTag(&Channel{Config: `{"provider":"azure","billing_tag":null}`}))
	// 同一个 key 重复出现、其中一次不是字符串：LoadConfig 同样会报错，拒绝。
	require.True(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":123,"billing_tag":"ok"}`}))
	// 别的字段类型不对不会掩盖非法标签，也不会让合法标签被拒绝。
	require.True(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":"bad tag","vertex_model_region":"oops"}`}))
	require.False(t, HasInvalidBillingTag(&Channel{Config: `{"billing_tag":"vertex-claude","vertex_model_region":"oops"}`}))
	// config 本身不是合法的 JSON 对象不属于标签校验的范围。
	require.False(t, HasInvalidBillingTag(&Channel{Config: `not json`}))
	require.False(t, HasInvalidBillingTag(&Channel{Config: `["billing_tag"]`}))
}

func TestDecodeChannelRatiosJSON(t *testing.T) {
	ratios, err := decodeChannelRatiosJSON(`{"48":0.9,"14":0,"abc":0.5,"tag:vertex-gemini":0.7,"tag:Vertex-Claude":0.85,"tag:bad tag":0.5,"tag:zero":0,"tag:":0.5}`)
	require.NoError(t, err)
	require.Equal(t, map[int]float64{48: 0.9}, ratios.Types)
	require.Equal(t, map[string]float64{"vertex-gemini": 0.7, "vertex-claude": 0.85}, ratios.Tags)

	// 同一标签有多种写法时以规范写法为准，结果不依赖 map 遍历顺序。
	for i := 0; i < 20; i++ {
		ratios, err = decodeChannelRatiosJSON(`{"tag:Vertex":0.5,"tag:vertex":0.8,"TAG: vertex ":0.6}`)
		require.NoError(t, err)
		require.Equal(t, map[string]float64{"vertex": 0.8}, ratios.Tags)
	}
	// 非正数的项先被忽略，不参与“规范写法优先”的比较。
	ratios, err = decodeChannelRatiosJSON(`{"tag:vertex":0,"tag:Vertex":0.5}`)
	require.NoError(t, err)
	require.Equal(t, map[string]float64{"vertex": 0.5}, ratios.Tags)

	_, err = decodeChannelRatiosJSON(`{"48":"0.9"}`)
	require.Error(t, err)
}

func TestUserChannelRatiosResolve(t *testing.T) {
	ratios, err := decodeChannelRatiosJSON(`{"48":0.9,"tag:vertex-gemini":0.7,"tag:vertex-claude":0.85}`)
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		channelType int
		tag         string
		ratio       float64
		matched     string
	}{
		{"标签命中，覆盖类型折扣", 48, "vertex-gemini", 0.7, "vertex-gemini"},
		{"同类型的另一个标签", 48, "vertex-claude", 0.85, "vertex-claude"},
		{"渠道有标签但用户未配置，回落类型折扣", 48, "vertex-other", 0.9, ""},
		{"渠道无标签，用类型折扣", 48, "", 0.9, ""},
		{"标签折扣不依赖渠道类型", 14, "vertex-claude", 0.85, "vertex-claude"},
		{"类型和标签都未配置", 14, "anthropic", 1.0, ""},
		{"未知渠道类型且无标签", 0, "", 1.0, ""},
	} {
		ratio, matched := ratios.Resolve(tc.channelType, tc.tag)
		require.Equal(t, tc.ratio, ratio, tc.name)
		require.Equal(t, tc.matched, matched, tc.name)
	}

	ratio, matched := UserChannelRatios{}.Resolve(48, "vertex-claude")
	require.Equal(t, 1.0, ratio)
	require.Empty(t, matched)
}

func TestUserChannelRatiosCachePayloadKeepsTags(t *testing.T) {
	ratios, err := decodeChannelRatiosJSON(`{"48":0.9,"tag:vertex-claude":0.85}`)
	require.NoError(t, err)
	decoded, err := decodeChannelRatiosJSON(ratios.encode())
	require.NoError(t, err)
	require.Equal(t, ratios, decoded)
	require.Equal(t, "", UserChannelRatios{}.encode())
}

// 返回给用户本人的 channel_ratios：只有计费实际用到的渠道类型折扣，标签项一律不带。
func TestStripBillingTagRatios(t *testing.T) {
	// key 用 JSON 的 Unicode 转义书写：原文里没有 "tag:"，JSON 解码后才是标签项。
	// 转义前缀（反斜杠加 u）用码点拼出来，保证写进测试输入的是转义序列本身。
	jsonEscape := string(rune(0x5C)) + "u"
	escapedPrefix := `{"48":0.9,"` + jsonEscape + `0074ag:vertex-claude":0.85}`
	escapedColon := `{"tag` + jsonEscape + `003avertex-claude":0.85}`
	require.NotContains(t, escapedPrefix, "tag:")
	require.NotContains(t, escapedColon, "tag:")

	for _, tc := range []struct{ input, want string }{
		{``, ``},
		{`{"48":0.9}`, `{"48":0.9}`},
		{`{"48":0.9,"tag:vertex-claude":0.85}`, `{"48":0.9}`},
		{`{"TAG: Vertex ":0.5,"14":0.8}`, `{"14":0.8}`},
		{`{"tag:vertex-claude":0.85}`, ``},
		{`tag: not json`, ``},
		{`not json`, ``},
		// 转义写法的标签项同样不返回
		{escapedPrefix, `{"48":0.9}`},
		{escapedColon, ``},
		// 无法识别的 key 和非正数对计费没有作用，不返回
		{`{"48":0.9,"abc":0.5,"14":0}`, `{"48":0.9}`},
		// 有一项的值不是数字时计费侧整份作废，这里也不返回
		{`{"48":0.9,"tag:vertex-claude":"0.85"}`, ``},
	} {
		require.Equal(t, tc.want, StripBillingTagRatios(tc.input), "input %q", tc.input)
	}
}

// 保存用户时的校验：计费侧会整份作废或永远不会命中的内容在保存时拒绝。
func TestValidateUserChannelRatios(t *testing.T) {
	for _, valid := range []string{
		``,
		`{}`,
		`{"48":0.9}`,
		`{"48":0.9,"tag:vertex-claude":0.85}`,
		// 写法不规范但标签合法，读取时会规范化
		`{"TAG: Vertex-Claude ":0.85}`,
		// 非正数和无法识别的 key 沿用既有约定：可以保存，读取时忽略
		`{"48":0,"abc":0.5}`,
	} {
		require.NoError(t, ValidateUserChannelRatios(valid), "input %q", valid)
	}
	for _, invalid := range []string{
		`not json`,
		`[]`,
		`"text"`,
		// 有一项的值不是数字：计费侧整份解析失败，渠道类型折扣也跟着失效
		`{"48":"0.9"}`,
		`{"48":0.9,"tag:vertex-claude":"0.85"}`,
		`{"48":0.9,"tag:vertex-claude":{"ratio":0.85}}`,
		// 标签名不合法：永远不会命中
		`{"48":0.9,"tag:Vertex Claude":0.85}`,
		`{"tag:":0.5}`,
		`{"tag:渠道":0.8}`,
		`{"tag:vertex-` + string(rune(0x212A)) + `laude":0.8}`,
		`{"tag:` + strings.Repeat("a", 65) + `":0.5}`,
	} {
		require.Error(t, ValidateUserChannelRatios(invalid), "input %q", invalid)
	}
}

func TestCacheGetUserChannelRatiosFromDB(t *testing.T) {
	db := setupTestDB(t, &User{})
	previous := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previous })

	require.NoError(t, db.Create(&User{Id: 1, Username: "tagged", ChannelRatios: `{"48":0.9,"tag:vertex-claude":0.85}`}).Error)
	require.NoError(t, db.Create(&User{Id: 2, Username: "legacy", AffCode: "legacy", AccessToken: "legacy", ChannelRatios: `{"48":0.9}`}).Error)
	require.NoError(t, db.Create(&User{Id: 3, Username: "broken", AffCode: "broken", AccessToken: "broken", ChannelRatios: `not json`}).Error)

	ratios, err := CacheGetUserChannelRatios(1)
	require.NoError(t, err)
	ratio, matched := ratios.Resolve(48, "vertex-claude")
	require.Equal(t, 0.85, ratio)
	require.Equal(t, "vertex-claude", matched)

	// 只有类型折扣的旧数据行为不变。
	ratios, err = CacheGetUserChannelRatios(2)
	require.NoError(t, err)
	ratio, matched = ratios.Resolve(48, "vertex-claude")
	require.Equal(t, 0.9, ratio)
	require.Empty(t, matched)

	for _, id := range []int{3, 404, 0} {
		ratios, err = CacheGetUserChannelRatios(id)
		require.NoError(t, err)
		ratio, _ = ratios.Resolve(48, "vertex-claude")
		require.Equal(t, 1.0, ratio)
	}

	// 同步选渠和异步结算共用的取值入口：读配置加取值一步完成，未登录或读不到配置时为 1.0。
	ratio, matched = ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 0.85, ratio)
	require.Equal(t, "vertex-claude", matched)
	ratio, matched = ResolveUserChannelRatio(2, 48, "vertex-claude")
	require.Equal(t, 0.9, ratio)
	require.Empty(t, matched)
	for _, id := range []int{3, 404, 0, -1} {
		ratio, matched = ResolveUserChannelRatio(id, 48, "vertex-claude")
		require.Equal(t, 1.0, ratio)
		require.Empty(t, matched)
	}
}

func TestListChannelBillingTags(t *testing.T) {
	db := setupTestDB(t, &Channel{})
	tags, err := ListChannelBillingTags()
	require.NoError(t, err)
	require.NotNil(t, tags, "empty result must serialize as [] instead of null")
	require.Empty(t, tags)

	for i, cfg := range []string{
		`{"billing_tag":"vertex-claude"}`,
		`{"billing_tag":" Vertex-Claude ","provider":"azure"}`,
		`{"billing_tag":"vertex-gemini"}`,
		`{"billing_tag":"bad tag"}`,
		`{"provider":"openai"}`,
		``,
		// 区域映射写坏的渠道照常计入它的标签
		`{"billing_tag":"vertex-gemini","vertex_model_region":"{bad json"}`,
	} {
		status := common.ChannelStatusEnabled
		if i == 1 {
			// 已禁用的渠道也计入：标签折扣配置不应因渠道临时禁用而从列表消失。
			status = common.ChannelStatusManuallyDisabled
		}
		require.NoError(t, db.Create(&Channel{Id: i + 1, Type: common.ChannelTypeVertexAI, Status: status, Config: cfg}).Error)
	}
	tags, err = ListChannelBillingTags()
	require.NoError(t, err)
	require.Equal(t, []BillingTagUsage{{Tag: "vertex-claude", ChannelCount: 2}, {Tag: "vertex-gemini", ChannelCount: 2}}, tags)
}
