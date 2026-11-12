package controller

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

func TestFileCapacityThresholds(t *testing.T) {
	db := setupTestDB(t)
	if err := db.AutoMigrate(&model.GroupConfig{}); err != nil {
		t.Fatal(err)
	}
	origUnit := config.QuotaPerUnit
	t.Cleanup(func() { config.QuotaPerUnit = origUnit })
	for _, unit := range []float64{500000, 1000} {
		config.QuotaPerUnit = unit
		for _, tc := range []struct {
			threshold int64
			limit     int64
		}{
			{0, 100 << 20}, {int64(5*unit) - 1, 100 << 20}, {int64(5 * unit), 300 << 20},
			{int64(50*unit) - 1, 300 << 20}, {int64(50 * unit), 500 << 20},
			{int64(100*unit) - 1, 500 << 20}, {int64(100 * unit), 1 << 30},
			{int64(250*unit) - 1, 1 << 30}, {int64(250 * unit), 2 << 30},
			{int64(300 * unit), 2 << 30}, {int64(500 * unit), 2 << 30}, {int64(1000 * unit), 2 << 30},
		} {
			row := model.GroupConfig{ID: 1, GroupKey: "arbitrary", UpgradeThreshold: tc.threshold}
			if err := db.Save(&row).Error; err != nil {
				t.Fatal(err)
			}
			if !CheckUserUsage(row.GroupKey, tc.limit) || CheckUserUsage(row.GroupKey, tc.limit+1) {
				t.Fatalf("unit=%v threshold=%d limit=%d", unit, tc.threshold, tc.limit)
			}
		}
	}
	if !CheckUserUsage("unknown", 100<<20) || CheckUserUsage("unknown", (100<<20)+1) {
		t.Fatal("unknown group capacity")
	}
}

func TestGetGroupsConfigOrder(t *testing.T) {
	db := setupTestDB(t)
	if err := db.AutoMigrate(&model.GroupConfig{}); err != nil {
		t.Fatal(err)
	}
	orig := common.GroupRatio
	t.Cleanup(func() { common.GroupRatio = orig })
	common.GroupRatio = map[string]float64{"Start": 1, "Build": 1, "Scale": 1, "Enterprise": 1, "z-custom": 1, "a-custom": 1}
	for i, key := range []string{"Start", "Build", "Scale", "Enterprise", "not-in-memory"} {
		if err := db.Create(&model.GroupConfig{GroupKey: key, SortOrder: i}).Error; err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	GetGroups(c)
	var body struct {
		Success bool
		Data    []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []string{"Start", "Build", "Scale", "Enterprise", "a-custom", "z-custom"}
	if !body.Success || !reflect.DeepEqual(body.Data, want) {
		t.Fatalf("got %+v, want %v", body, want)
	}
}
