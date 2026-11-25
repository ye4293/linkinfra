package model

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/redistest"
	"github.com/stretchr/testify/require"
)

// 保存时的规范化和校验：来源名去空格、转小写；重复、冲突、超限、only 没有 order 都拒绝。
func TestTokenProviderRoutingNormalize(t *testing.T) {
	normalized, err := TokenProviderRouting{
		Order:  []string{" Vertex ", "ANTHROPIC"},
		Only:   true,
		Ignore: []string{"OpenRouter", " aws "},
	}.Normalize()
	require.NoError(t, err)
	require.Equal(t, TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true, Ignore: []string{"aws", "openrouter"}}, normalized,
		"order 保留用户给的顺序，ignore 排序")

	// 没有填任何来源等于没有设置路由。
	normalized, err = TokenProviderRouting{Order: []string{}, Ignore: []string{}}.Normalize()
	require.NoError(t, err)
	require.True(t, normalized.IsEmpty())
	require.Nil(t, normalized.Order)
	require.Nil(t, normalized.Ignore)

	tooMany := make([]string, providerRoutingMaxEntries+1)
	for i := range tooMany {
		tooMany[i] = "p" + strings.Repeat("x", i)
	}
	for name, tc := range map[string]struct {
		routing TokenProviderRouting
		message string
	}{
		"order 里大小写不同的重复": {TokenProviderRouting{Order: []string{"vertex", " VERTEX"}}, "more than once"},
		"ignore 里的重复":     {TokenProviderRouting{Ignore: []string{"aws", "aws"}}, "more than once"},
		"同一来源既选中又忽略":      {TokenProviderRouting{Order: []string{"vertex"}, Ignore: []string{"Vertex"}}, `both "order" and "ignore"`},
		"only 而没有 order":  {TokenProviderRouting{Only: true}, `requires at least one provider`},
		"only 且只有 ignore": {TokenProviderRouting{Only: true, Ignore: []string{"aws"}}, `requires at least one provider`},
		"空的来源名":           {TokenProviderRouting{Order: []string{"vertex", "  "}}, "empty provider name"},
		"来源名超长":           {TokenProviderRouting{Ignore: []string{strings.Repeat("a", providerNameMaxLength+1)}}, "longer than"},
		"order 超过 32 个":   {TokenProviderRouting{Order: tooMany}, "more than 32"},
		"ignore 超过 32 个":  {TokenProviderRouting{Ignore: tooMany}, "more than 32"},
	} {
		_, err := tc.routing.Normalize()
		require.ErrorContains(t, err, tc.message, name)
	}

	// 上限以内可以保存。
	_, err = TokenProviderRouting{Order: tooMany[:providerRoutingMaxEntries], Ignore: []string{strings.Repeat("a", providerNameMaxLength)}}.Normalize()
	require.NoError(t, err)
}

// 请求里提交的 provider_routing：只接受含 order / only / ignore 的对象，拼错的字段不会被静默忽略。
func TestParseTokenProviderRouting(t *testing.T) {
	routing, err := ParseTokenProviderRouting([]byte(` {"order":["Vertex"],"only":true,"ignore":["AWS"]} `))
	require.NoError(t, err)
	require.Equal(t, TokenProviderRouting{Order: []string{"vertex"}, Only: true, Ignore: []string{"aws"}}, routing)

	routing, err = ParseTokenProviderRouting([]byte(`{}`))
	require.NoError(t, err)
	require.True(t, routing.IsEmpty(), "空对象表示清空")

	for name, raw := range map[string]string{
		"拼错的字段":      `{"order":["vertex"],"ingore":["aws"]}`,
		"多余的字段":      `{"order":["vertex"],"sort":"price"}`,
		"only 不是布尔值": `{"order":["vertex"],"only":"true"}`,
		"order 不是数组": `{"order":"vertex"}`,
		"来源名不是字符串":   `{"ignore":[1]}`,
		"数组":         `["vertex"]`,
		"字符串":        `"vertex"`,
		"空内容":        ``,
		"对象后面还有内容":   `{"order":["vertex"]} {"ignore":["vertex"]}`,
		"校验不通过":      `{"only":true}`,
		"null":       `null`,
	} {
		_, err := ParseTokenProviderRouting([]byte(raw))
		require.Error(t, err, name)
	}
}

// 允许范围和分层：ignore 优先，其次 only；order 里每个来源一层，only 关闭时其余来源是最后一层。
func TestTokenProviderRoutingAllowsAndLayers(t *testing.T) {
	type expect struct {
		layers int
		layer  map[string]int // 来源 → 所在层，-1 表示不允许
	}
	for name, tc := range map[string]struct {
		routing TokenProviderRouting
		expect  expect
	}{
		"没有设置": {TokenProviderRouting{}, expect{1, map[string]int{"vertex": 0, "anthropic": 0, "aws": 0}}},
		"只有 ignore": {TokenProviderRouting{Ignore: []string{"aws"}},
			expect{1, map[string]int{"vertex": 0, "anthropic": 0, "aws": -1}}},
		"order 且 only 关闭": {TokenProviderRouting{Order: []string{"vertex"}},
			expect{2, map[string]int{"vertex": 0, "anthropic": 1, "aws": 1}}},
		"order 且 only 打开": {TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true},
			expect{2, map[string]int{"vertex": 0, "anthropic": 1, "aws": -1}}},
		"order、only 关闭、ignore": {TokenProviderRouting{Order: []string{"anthropic", "vertex"}, Ignore: []string{"aws"}},
			expect{3, map[string]int{"anthropic": 0, "vertex": 1, "aws": -1, "azure": 2}}},
		// 只可能来自直接改库：only 打开而没有 order 时任何来源都不允许，不会放开成没有限制。
		"only 打开而没有 order": {TokenProviderRouting{Only: true}, expect{1, map[string]int{"vertex": -1, "aws": -1}}},
	} {
		require.Equal(t, tc.expect.layers, tc.routing.layerCount(), name)
		for source, layer := range tc.expect.layer {
			require.Equal(t, layer, tc.routing.layerOf(source), "%s: %s", name, source)
			require.Equal(t, layer >= 0, tc.routing.Allows(source), "%s: %s", name, source)
		}
	}
}

// 存储：没有设置路由时写 NULL，读回来是零值；写入的内容规范、可读回；
// 读取不拒绝未知字段（回滚到旧版本后仍能读出），内容损坏时报错而不是当成没有设置。
func TestTokenProviderRoutingStorage(t *testing.T) {
	db := setupTestDB(t, &Token{})
	rawColumn := func(id int) sql.NullString {
		t.Helper()
		var raw sql.NullString
		require.NoError(t, db.Raw("SELECT provider_routing FROM tokens WHERE id = ?", id).Scan(&raw).Error)
		return raw
	}

	plain := Token{UserId: 1, Key: "plain", Name: "plain"}
	require.NoError(t, plain.Insert())
	require.False(t, rawColumn(plain.Id).Valid, "没有设置路由时写 NULL")

	routed := Token{UserId: 1, Key: "routed", Name: "routed", ProviderRouting: TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true, Ignore: []string{"aws"}}}
	require.NoError(t, routed.Insert())
	require.JSONEq(t, `{"order":["vertex","anthropic"],"only":true,"ignore":["aws"]}`, rawColumn(routed.Id).String)

	loaded, err := GetTokenById(routed.Id)
	require.NoError(t, err)
	require.Equal(t, routed.ProviderRouting, loaded.ProviderRouting)
	loaded, err = GetTokenById(plain.Id)
	require.NoError(t, err)
	require.True(t, loaded.ProviderRouting.IsEmpty())

	// UpdateWithProviderRouting 写入修改后的路由；清空时写回 NULL，其他列不受影响。
	routed.ProviderRouting = TokenProviderRouting{Ignore: []string{"aws"}}
	routed.Name = "renamed"
	require.NoError(t, routed.UpdateWithProviderRouting())
	require.JSONEq(t, `{"order":[],"only":false,"ignore":["aws"]}`, rawColumn(routed.Id).String)

	// Update 不写路由这一列：内存里的值不同（例如并发的另一个请求读到的旧值）也不会覆盖库里的路由。
	stale := routed
	stale.ProviderRouting = TokenProviderRouting{Order: []string{"vertex"}, Only: true}
	stale.Name = "renamed-again"
	require.NoError(t, stale.Update())
	require.JSONEq(t, `{"order":[],"only":false,"ignore":["aws"]}`, rawColumn(routed.Id).String)
	stale.ProviderRouting = TokenProviderRouting{}
	require.NoError(t, stale.Update())
	require.True(t, rawColumn(routed.Id).Valid, "没有提交路由的更新不会把它清空")

	routed.ProviderRouting = TokenProviderRouting{}
	routed.Name = "renamed"
	require.NoError(t, routed.UpdateWithProviderRouting())
	require.False(t, rawColumn(routed.Id).Valid, "清空路由时写回 NULL")
	loaded, err = GetTokenById(routed.Id)
	require.NoError(t, err)
	require.Equal(t, "renamed", loaded.Name)
	require.True(t, loaded.ProviderRouting.IsEmpty())

	// 只更新状态和访问时间的写入不动路由。
	require.NoError(t, db.Exec("UPDATE tokens SET provider_routing = ? WHERE id = ?", `{"ignore":["aws"],"future_field":1}`, plain.Id).Error)
	plain.Status = common.TokenStatusExpired
	require.NoError(t, plain.SelectUpdate())
	loaded, err = GetTokenById(plain.Id)
	require.NoError(t, err)
	require.Equal(t, common.TokenStatusExpired, loaded.Status)
	require.Equal(t, TokenProviderRouting{Ignore: []string{"aws"}}, loaded.ProviderRouting, "未知字段不影响读取")

	// 库里的内容不是合法 JSON：读取报错，不把它当成没有设置路由。
	require.NoError(t, db.Exec("UPDATE tokens SET provider_routing = ? WHERE id = ?", `{"ignore":`, plain.Id).Error)
	_, err = GetTokenById(plain.Id)
	require.ErrorContains(t, err, "provider_routing")
}

// 发布期间有实例先于主节点（负责加列）启动时，provider_routing 这一列还不存在：
// 读取、创建和更新没有设置路由的令牌都照常工作，只有写入路由会失败。
func TestTokensWorkBeforeProviderRoutingColumnExists(t *testing.T) {
	db := setupTestDB(t, &Token{})
	require.NoError(t, db.Migrator().DropColumn(&Token{}, "provider_routing"))
	require.False(t, db.Migrator().HasColumn(&Token{}, "provider_routing"))

	token := Token{UserId: 1, Key: "nocolumn", Name: "before", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, token.Insert())
	loaded, err := ValidateUserToken("nocolumn")
	require.NoError(t, err)
	require.True(t, loaded.ProviderRouting.IsEmpty())

	token.Name = "after"
	require.NoError(t, token.Update())
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.SelectUpdate())
	loaded, err = GetTokenByIds(token.Id, token.UserId)
	require.NoError(t, err)
	require.Equal(t, "after", loaded.Name)
	require.Equal(t, common.TokenStatusDisabled, loaded.Status)

	// 写入路由需要这一列：报错，不会静默丢掉用户的设置。
	token.ProviderRouting = TokenProviderRouting{Ignore: []string{"aws"}}
	require.Error(t, token.UpdateWithProviderRouting())
	routed := Token{UserId: 1, Key: "nocolumn-routed", Name: "routed", ProviderRouting: TokenProviderRouting{Ignore: []string{"aws"}}}
	require.Error(t, routed.Insert())
	require.NoError(t, DeleteTokenById(token.Id, token.UserId))
}

// 接口返回的 JSON 固定带三个字段，列表为空时是 []。
func TestTokenProviderRoutingJSON(t *testing.T) {
	raw, err := json.Marshal(Token{Id: 1})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"provider_routing":{"order":[],"only":false,"ignore":[]}`)

	raw, err = json.Marshal(Token{Id: 1, ProviderRouting: TokenProviderRouting{Order: []string{"vertex"}, Only: true}})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"provider_routing":{"order":["vertex"],"only":true,"ignore":[]}`)

	// 令牌缓存存的就是这份 JSON，读回来路由不变（空列表统一成 nil）。
	var cached Token
	require.NoError(t, json.Unmarshal(raw, &cached))
	require.Equal(t, TokenProviderRouting{Order: []string{"vertex"}, Only: true}, cached.ProviderRouting)
	require.True(t, cached.ProviderRouting.Allows("vertex"))
	require.False(t, cached.ProviderRouting.Allows("aws"))

	// Equal 比较内容：空列表和 nil 相同，order 的顺序有含义。
	base := TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true, Ignore: []string{"aws"}}
	require.True(t, base.Equal(TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true, Ignore: []string{"aws"}}))
	require.True(t, TokenProviderRouting{}.Equal(TokenProviderRouting{Order: []string{}, Ignore: []string{}}))
	for name, other := range map[string]TokenProviderRouting{
		"order 的顺序不同":  {Order: []string{"anthropic", "vertex"}, Only: true, Ignore: []string{"aws"}},
		"only 不同":      {Order: []string{"vertex", "anthropic"}, Ignore: []string{"aws"}},
		"ignore 不同":    {Order: []string{"vertex", "anthropic"}, Only: true, Ignore: []string{"azure"}},
		"少了 ignore":    {Order: []string{"vertex", "anthropic"}, Only: true},
		"order 少了一个来源": {Order: []string{"vertex"}, Only: true, Ignore: []string{"aws"}},
		"没有设置":         {},
	} {
		require.False(t, base.Equal(other), name)
		require.False(t, other.Equal(base), name)
	}
}

// Redis 开启时的令牌缓存：缓存里带来源路由；不读旧版本实例留下的缓存（旧 key，不含路由）；
// 更新和删除令牌时新旧两个 key 都清掉，修改立即生效。
func TestTokenCacheCarriesProviderRoutingAndIsInvalidated(t *testing.T) {
	db := setupTestDB(t, &Token{})
	fake := redistest.Use(t)
	token := Token{UserId: 1, Key: "cachedkey", Name: "cached", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true,
		ProviderRouting: TokenProviderRouting{Ignore: []string{"aws"}}}
	require.NoError(t, token.Insert())
	// 旧版本实例写的缓存没有路由字段，新版本不能读它。
	fake.Set("token:cachedkey", `{"id":1,"user_id":1,"key":"cachedkey","status":1,"expired_time":-1,"unlimited_quota":true}`)

	loaded, err := ValidateUserToken("cachedkey")
	require.NoError(t, err)
	require.Equal(t, TokenProviderRouting{Ignore: []string{"aws"}}, loaded.ProviderRouting)
	cached, ok := fake.Get("token:v2:cachedkey")
	require.True(t, ok)
	require.Contains(t, cached, `"ignore":["aws"]`)
	require.Positive(t, fake.TTL("token:v2:cachedkey"))

	// 命中缓存：绕过接口直接改库不会立即生效。
	require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Update("provider_routing", nil).Error)
	loaded, err = ValidateUserToken("cachedkey")
	require.NoError(t, err)
	require.Equal(t, []string{"aws"}, loaded.ProviderRouting.Ignore)

	// 通过更新接口修改：新旧两个 key 都清掉，下一次读取就是新的路由。
	token.ProviderRouting = TokenProviderRouting{Order: []string{"vertex"}, Only: true}
	require.NoError(t, token.UpdateWithProviderRouting())
	for _, key := range []string{"token:v2:cachedkey", "token:cachedkey"} {
		_, ok = fake.Get(key)
		require.False(t, ok, key)
	}
	loaded, err = ValidateUserToken("cachedkey")
	require.NoError(t, err)
	require.Equal(t, TokenProviderRouting{Order: []string{"vertex"}, Only: true}, loaded.ProviderRouting)

	// 禁用令牌立即生效，不再等缓存过期。
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.Update())
	_, err = ValidateUserToken("cachedkey")
	require.Error(t, err)

	// 删除（单个和批量）都清缓存。
	token.Status = common.TokenStatusEnabled
	require.NoError(t, token.Update())
	_, err = ValidateUserToken("cachedkey")
	require.NoError(t, err)
	require.NoError(t, DeleteTokenById(token.Id, token.UserId))
	_, ok = fake.Get("token:v2:cachedkey")
	require.False(t, ok)
	_, err = ValidateUserToken("cachedkey")
	require.Error(t, err)

	batch := Token{UserId: 1, Key: "batchkey", Name: "batch", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, batch.Insert())
	_, err = ValidateUserToken("batchkey")
	require.NoError(t, err)
	_, ok = fake.Get("token:v2:batchkey")
	require.True(t, ok)
	require.NoError(t, DeleteTokensByIds([]int{batch.Id}, batch.UserId))
	_, ok = fake.Get("token:v2:batchkey")
	require.False(t, ok)
	_, err = ValidateUserToken("batchkey")
	require.Error(t, err)
}
