package common

import "testing"

// TestGeneratePasswordNotRepeating 密码重置生成的新密码不能重复。
//
// GeneratePassword 用于「忘记密码」流程（controller/misc.go:160），
// 生成的 8 位密码会直接写库并邮件发给用户，属于凭据。
//
// 原实现用 rand.NewSource(time.Now().UnixNano()) 播种 math/rand：
// Windows 时钟精度约 0.5~15ms，同一 tick 内的两次重置会拿到相同种子、
// 生成**完全相同的密码**。攻击者只要知道大致时间就能大幅缩小猜测空间。
//
// 这与本轮已修的 helper.GetRandomString 是同一类缺陷，只是写法不同
// （NewSource 而非 Seed），当时没被 grep 到。
func TestGeneratePasswordNotRepeating(t *testing.T) {
	const n = 50
	seen := make(map[string]int, n)
	for i := 0; i < n; i++ {
		p := GeneratePassword()
		if len(p) != 8 {
			t.Fatalf("密码长度 = %d, want 8", len(p))
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("第 %d 次与第 %d 次生成了相同的密码 —— "+
				"检查是否仍在用 wall-clock 播种的 math/rand", i+1, prev+1)
		}
		seen[p] = i
	}
}
