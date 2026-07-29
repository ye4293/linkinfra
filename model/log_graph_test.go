package model

import (
	"testing"
	"time"
)

// TestHourBucketExprBehavior 实测 hourBucketExpr 在真实 SQL 引擎下的行为。
//
// 这个表达式是本次为替换 MySQL 专有的 HOUR(FROM_UNIXTIME()) 而新引入的，
// 不是纯删除，所以必须实测而不能只靠推理。至少在 sqlite 上验证：
//   - 取模与减法产生的桶起点确实是整小时对齐
//   - SELECT 里的别名与 GROUP BY 的表达式能匹配上
//   - 负数 created_at（理论上不该出现，但要知道行为）
// TestFillHourlyDataAccumulates 同一个「HH」标签收到多个桶时必须累加，不能覆盖。
//
// 旧实现的 SQL 分组键就是小时标签本身（LPAD(HOUR(...))），跨天的同小时由
// SQL 求和，Go 侧只是填槽。新实现的分组键换成了桶起点（含日期信息），比
// 标签键更细 —— 一旦有两个不同日期的桶映射到同一个 "HH"，用 = 赋值就是
// 后者覆盖前者、前者的量静默丢失，属于对旧语义的回退。
//
// 触发路径：applyLogIdRange 走的是 findMaxIdByTimestampGeneric 的二分查找，
// 它假设 created_at 随 id 单调。并发写入下边界会漂；若表里存在
// created_at = 0 的行（默认值或导入数据），单调假设彻底失效，
// id 区间可能横跨多天。
func TestFillHourlyDataAccumulates(t *testing.T) {
	hourlyData := newEmptyHourlyData()

	day := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	prevDay := day.Add(-24 * time.Hour)

	// 两个不同日期的桶，都映射到 "10"
	rows := []hourBucketRow{
		{Bucket: day.Add(10 * time.Hour).Unix(), Amount: 30},
		{Bucket: prevDay.Add(10 * time.Hour).Unix(), Amount: 7},
	}
	fillHourlyData(hourlyData, rows)

	var got int64
	for _, d := range hourlyData {
		if d.Hour == "10" {
			got = d.Amount
		}
	}
	if got != 37 {
		t.Errorf("10 点 = %d, want 37（两个桶应累加，用 = 赋值会丢掉先填的那个）", got)
	}
}

func TestHourBucketExprBehavior(t *testing.T) {
	setupTestDB(t, &Log{})

	// 造 3 条日志：同一小时内两条、下一小时一条
	base := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC).Unix()
	rows := []Log{
		{UserId: 1, CreatedAt: base + 5, Type: LogTypeConsume, Quota: 10},
		{UserId: 1, CreatedAt: base + 3599, Type: LogTypeConsume, Quota: 20},
		{UserId: 1, CreatedAt: base + 3600, Type: LogTypeConsume, Quota: 40},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create log failed: %v", err)
		}
	}

	type probe struct {
		Bucket int64 `gorm:"column:bucket"`
		Amount int64 `gorm:"column:amount"`
	}
	var got []probe
	err := DB.Model(&Log{}).
		Select("COALESCE(SUM(quota), 0) as amount, " + hourBucketExpr + " as bucket").
		Group(hourBucketExpr).
		Order("bucket asc").
		Scan(&got).Error
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("分组数 = %d, want 2；实际结果 %+v", len(got), got)
	}

	// 第一桶：base 所在整点，10+20=30
	if got[0].Bucket != base {
		t.Errorf("第一桶起点 = %d, want %d（应对齐到整小时）", got[0].Bucket, base)
	}
	if got[0].Amount != 30 {
		t.Errorf("第一桶金额 = %d, want 30", got[0].Amount)
	}
	// 第二桶：base+3600，40
	if got[1].Bucket != base+3600 {
		t.Errorf("第二桶起点 = %d, want %d", got[1].Bucket, base+3600)
	}
	if got[1].Amount != 40 {
		t.Errorf("第二桶金额 = %d, want 40", got[1].Amount)
	}

	// 桶起点换算成 UTC 小时后必须落在 00~23
	for _, r := range got {
		h := time.Unix(r.Bucket, 0).UTC().Hour()
		if h < 0 || h > 23 {
			t.Errorf("桶 %d 换算出的小时 %d 越界", r.Bucket, h)
		}
	}
	t.Logf("桶结果: %+v", got)
}

// TestGetAllGraphEndToEnd 走完整的 GetAllGraph 路径，确认返回 24 个槽位
// 且数据落在正确的小时上。
func TestGetAllGraphEndToEnd(t *testing.T) {
	setupTestDB(t, &Log{})

	day := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	// 10 点两条、13 点一条
	rows := []Log{
		{UserId: 1, CreatedAt: day.Add(10 * time.Hour).Unix(), Type: LogTypeConsume, Quota: 10},
		{UserId: 1, CreatedAt: day.Add(10*time.Hour + 30*time.Minute).Unix(), Type: LogTypeConsume, Quota: 20},
		{UserId: 1, CreatedAt: day.Add(13 * time.Hour).Unix(), Type: LogTypeConsume, Quota: 40},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create log failed: %v", err)
		}
	}

	data, err := GetAllGraph(day.Unix(), "quota")
	if err != nil {
		t.Fatalf("GetAllGraph failed: %v", err)
	}
	if len(data) != 24 {
		t.Fatalf("槽位数 = %d, want 24", len(data))
	}

	byHour := map[string]int64{}
	for _, d := range data {
		byHour[d.Hour] = d.Amount
	}
	if byHour["10"] != 30 {
		t.Errorf("10 点 = %d, want 30", byHour["10"])
	}
	if byHour["13"] != 40 {
		t.Errorf("13 点 = %d, want 40", byHour["13"])
	}
	if byHour["11"] != 0 {
		t.Errorf("11 点 = %d, want 0", byHour["11"])
	}
	// 格式必须是两位零填充，前端靠字符串相等匹配
	if data[0].Hour != "00" || data[9].Hour != "09" {
		t.Errorf("小时格式错误: data[0]=%q data[9]=%q, want \"00\" / \"09\"", data[0].Hour, data[9].Hour)
	}
}
