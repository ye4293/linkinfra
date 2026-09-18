package util

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/stretchr/testify/require"
)

func TestModelDiscountBillingFactors(t *testing.T) {
	previous, groups := common.ModelDiscountsJSON(), common.GroupRatio
	t.Cleanup(func() {
		require.NoError(t, common.UpdateModelDiscounts(previous))
		common.GroupRatio = groups
	})
	require.NoError(t, common.UpdateModelDiscounts(`{"actual":0.5,"alias":0.9}`))
	common.GroupRatio = map[string]float64{"test": 0.8}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_discount", 0.6)
	c.Set("user_channel_ratio", 0.7)
	require.InDelta(t, 0.8*0.6*0.7*0.5, GetBillingGroupRatio(c, "test", "actual"), 1e-12)
	meta := &RelayMeta{Group: "test", OriginModelName: "alias", ActualModelName: "actual", ChannelDiscount: 0.6, UserChannelRatio: 0.7}
	require.InDelta(t, 0.8*0.6*0.7*0.5, meta.CombinedGroupRatio(), 1e-12)
	require.InDelta(t, 0.8*0.5, GetAsyncBillingGroupRatio("test", 0, 0, 0, "actual"), 1e-12)
	require.InDelta(t, 0.8*0.6*0.7, GetBillingGroupRatio(c, "test", "unconfigured"), 1e-12)
}
