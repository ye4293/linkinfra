package helper

import "testing"

// TestGetRandomStringNotRepeating 连续调用必须返回不同的串。
//
// 原实现在每次调用里都执行 rand.Seed(time.Now().UnixNano())。Windows 的
// 时钟精度约 0.5~15ms，同一个 tick 内的多次调用拿到相同种子、返回完全
// 相同的结果 —— 实测连续 5 次 GetRandomString(4) 全部返回 "CXo1"。
//
// 这直接导致：aff_code（有 uniqueIndex）在同一 tick 内注册的两个用户
// 会撞码、appOrderId（充值订单号，也是邀请返现的幂等键）会撞号、
// OAuth state（CSRF 防护参数）会撞值。
//
// 这条测试守住「不要把 rand.Seed 加回来」。
func TestGetRandomStringNotRepeating(t *testing.T) {
	const n = 50
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		s := GetRandomString(8)
		if prev, dup := seen[s]; dup {
			t.Fatalf("第 %d 次与第 %d 次返回了相同的串 %q —— "+
				"检查是否有人重新加入了 rand.Seed 调用", i+1, prev+1, s)
		}
		seen[s] = i
	}
}

// TestGetRandomStringLength 长度参数生效，且字符都在字符集内。
func TestGetRandomStringLength(t *testing.T) {
	for _, n := range []int{1, 4, 8, 16, 32} {
		s := GetRandomString(n)
		if len(s) != n {
			t.Errorf("GetRandomString(%d) 长度 = %d", n, len(s))
		}
		for _, c := range s {
			found := false
			for _, k := range keyChars {
				if c == k {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("GetRandomString(%d) 含字符集外的字符 %q", n, c)
			}
		}
	}
}

// TestGetRandomNumberStringNotRepeating 同上，数字串也不能重复。
func TestGetRandomNumberStringNotRepeating(t *testing.T) {
	const n = 30
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		// 12 位数字，空间 1e12，30 次取样重复的概率可以忽略
		s := GetRandomNumberString(12)
		if seen[s] {
			t.Fatalf("第 %d 次返回了重复的数字串 %q", i+1, s)
		}
		seen[s] = true
	}
}

// TestGenerateKeyNotRepeating token key 也不能重复。
func TestGenerateKeyNotRepeating(t *testing.T) {
	const n = 30
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		k := GenerateKey()
		if len(k) != 48 {
			t.Fatalf("GenerateKey 长度 = %d, want 48", len(k))
		}
		if seen[k] {
			t.Fatalf("第 %d 次返回了重复的 key", i+1)
		}
		seen[k] = true
	}
}
