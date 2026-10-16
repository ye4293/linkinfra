package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestInvalidAffinityDoesNotCarryKeyOrRetryRestriction(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("cached_key_index", 3)
	c.Set(ginKeyAffinitySkipRetry, true)
	InvalidateChannelAffinity(c, "channel_disabled")
	require.Equal(t, -1, c.GetInt("cached_key_index"))
	require.False(t, ShouldSkipRetryAfterChannelAffinityFailure(c))
}
