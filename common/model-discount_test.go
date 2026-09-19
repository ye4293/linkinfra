package common

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestModelDiscountValidationAndDefaults(t *testing.T) {
	previous := ModelDiscountsJSON()
	t.Cleanup(func() { require.NoError(t, UpdateModelDiscounts(previous)) })
	require.NoError(t, UpdateModelDiscounts(`{"discounted":0.53,"regular":1}`))
	require.Equal(t, 1.0, GetModelDiscount("missing"))
	require.Equal(t, 1.0, GetModelDiscount("regular"))
	require.Equal(t, 0.53, GetModelDiscount("discounted"))
	for _, invalid := range []string{`null`, `[]`, `{"x":null}`, `{"x":0}`, `{"x":-0.1}`, `{"x":1.1}`, `{"x":"0.5"}`, `{" x":0.5}`, `{"":0.5}`} {
		require.Error(t, UpdateModelDiscounts(invalid), invalid)
		require.Equal(t, 0.53, GetModelDiscount("discounted"))
	}
	copy := GetModelDiscounts()
	copy["discounted"] = 1
	require.Equal(t, 0.53, GetModelDiscount("discounted"))
}
