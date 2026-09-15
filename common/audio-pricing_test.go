package common

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAudioDurationPricesValidation(t *testing.T) {
	previous := AudioDurationPricesJSON()
	t.Cleanup(func() { require.NoError(t, UpdateAudioDurationPrices(previous)) })
	require.NoError(t, UpdateAudioDurationPrices(`{"gpt-transcribe":0.0045,"free-model":0}`))
	price, enabled := GetAudioDurationPrice("free-model")
	assert.True(t, enabled)
	assert.Zero(t, price)
	_, enabled = GetAudioDurationPrice("unconfigured")
	assert.False(t, enabled)
	for _, input := range []string{`null`, `[]`, `{"a":null}`, `{"a":-1}`, `{"a":"0.0045"}`, `{"a":1e999}`, `{"":1}`, `{" a ":1}`} {
		t.Run(input, func(t *testing.T) {
			require.Error(t, UpdateAudioDurationPrices(input))
			price, enabled := GetAudioDurationPrice("gpt-transcribe")
			assert.True(t, enabled)
			assert.Equal(t, 0.0045, price)
		})
	}
	require.NoError(t, UpdateAudioDurationPrices(`{}`))
	_, enabled = GetAudioDurationPrice("gpt-transcribe")
	assert.False(t, enabled)
}
