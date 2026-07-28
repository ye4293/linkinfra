package model

import (
	"fmt"
	"sort"
)

// LevelChangeSample 一条等级变化的样本，用于人工抽查。
type LevelChangeSample struct {
	UserId     int    `json:"user_id"`
	Username   string `json:"username"`
	FromGroup  string `json:"from_group"`
	ToGroup    string `json:"to_group"`
	TopupQuota int64  `json:"topup_quota"`
}

// LevelRecalcPreview 等级重算的影响面预览。
type LevelRecalcPreview struct {
	TotalUsers   int                 `json:"total_users"`
	ChangedUsers int                 `json:"changed_users"`
	Transitions  map[string]int      `json:"transitions"` // "Lv1 -> Lv4" => 人数
	Samples      []LevelChangeSample `json:"samples"`
}

// previewSampleLimit 样本上限，避免大库下把内存和日志撑爆。
const previewSampleLimit = 20

// PreviewLevelRecalc 只读地计算「若现在对所有用户执行等级重算，谁会变、变成什么」。
//
// 存在的意义：RecalcUserLevel 会在每笔充值后自动跑，而历史上
// controller/stripeCharge.go 里的 UserLevelUpgrade 因两处 bug 从未真正生效
// （条件写反 + 从无登录态的 webhook context 取 userId）。这意味着上线后
// 会有一批用户被"补"到本该早就到达的等级，折扣随之变低。
//
// 运营需要在改动实际发生之前看到这个影响面，因此本函数严格只读：
// 不写用户表、不写日志、不动缓存。
//
// 判定逻辑必须与 RecalcUserLevel 保持一致，否则预览就失去意义。
func PreviewLevelRecalc() (*LevelRecalcPreview, error) {
	preview := &LevelRecalcPreview{
		Transitions: map[string]int{},
		Samples:     []LevelChangeSample{},
	}

	configs, err := GetGroupConfigsByThresholdDesc()
	if err != nil {
		return nil, err
	}

	var users []User
	if err := DB.Find(&users).Error; err != nil {
		return nil, err
	}
	preview.TotalUsers = len(users)

	if len(configs) == 0 {
		// 与 RecalcUserLevel 一致：分组表为空时不做任何判断
		return preview, nil
	}

	// 与 RecalcUserLevel 相同的门槛索引，避免两处逻辑漂移
	thresholdOf := make(map[string]int64, len(configs))
	for i := range configs {
		thresholdOf[configs[i].GroupKey] = configs[i].UpgradeThreshold
	}

	for i := range users {
		u := &users[i]

		var target *GroupConfig
		for j := range configs {
			if u.TopupQuota >= configs[j].UpgradeThreshold {
				target = &configs[j]
				break
			}
		}
		if target == nil || target.GroupKey == u.Group {
			continue
		}
		// 只升不降：当前分组不在表中时视为门槛 0
		if target.UpgradeThreshold <= thresholdOf[u.Group] {
			continue
		}

		preview.ChangedUsers++
		preview.Transitions[fmt.Sprintf("%s -> %s", u.Group, target.GroupKey)]++
		if len(preview.Samples) < previewSampleLimit {
			preview.Samples = append(preview.Samples, LevelChangeSample{
				UserId:     u.Id,
				Username:   u.Username,
				FromGroup:  u.Group,
				ToGroup:    target.GroupKey,
				TopupQuota: u.TopupQuota,
			})
		}
	}

	return preview, nil
}

// FormatLevelRecalcPreview 把预览结果渲染成人类可读的报告。
func FormatLevelRecalcPreview(p *LevelRecalcPreview) string {
	out := "=== 等级重算影响面预览（只读，未修改任何数据）===\n"
	out += fmt.Sprintf("用户总数:   %d\n", p.TotalUsers)
	out += fmt.Sprintf("将会变更:   %d\n", p.ChangedUsers)

	if p.ChangedUsers == 0 {
		out += "\n无用户等级会发生变化。\n"
		return out
	}

	out += "\n--- 变更分布 ---\n"
	keys := make([]string, 0, len(p.Transitions))
	for k := range p.Transitions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out += fmt.Sprintf("  %-16s %d 人\n", k, p.Transitions[k])
	}

	out += fmt.Sprintf("\n--- 样本（最多 %d 条）---\n", previewSampleLimit)
	for _, s := range p.Samples {
		out += fmt.Sprintf("  #%d %-20s %s -> %-6s 累计充值 %d\n",
			s.UserId, s.Username, s.FromGroup, s.ToGroup, s.TopupQuota)
	}
	return out
}
