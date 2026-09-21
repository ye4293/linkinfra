package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

func stateContext(user int, body string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	c.Set("id", user)
	return c
}

func stateOutputContext(user, channel int, provider string) *gin.Context {
	c := stateContext(user, `{}`)
	c.Set("channel_id", channel)
	c.Set("key_index", 0)
	c.Request = c.Request.WithContext(model.WithRetryProvider(c.Request.Context(), provider))
	return c
}

func TestResponsesThinkingBindsNextTurnWithoutSessionHeader(t *testing.T) {
	for _, provider := range []string{"openai", "azure"} {
		t.Run(provider, func(t *testing.T) {
			store := newMemoryResponsesStateStore(100)
			output := stateOutputContext(1, 10, provider)
			require.NoError(t, rememberResponsesState(output, []byte(`{"id":"resp_a","output":[{"type":"reasoning","id":"rs_a","encrypted_content":"secret-thinking","summary":[]},{"type":"function_call","id":"fc_a","call_id":"call_1","name":"read_file","arguments":"{}"}]}`), false, store))
			body := `{"model":"gpt-test","input":[{"type":"reasoning","id":"rs_a","encrypted_content":"secret-thinking","summary":[]},{"type":"function_call","id":"fc_a","call_id":"call_1","name":"read_file","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"file contents"}]}`
			next := stateContext(1, body)
			require.NoError(t, bindResponsesState(next, store))
			require.Equal(t, provider, model.RetryProvider(next.Request.Context()))
			require.Zero(t, model.ResponseStateChannel(next.Request.Context()), "compatible resources may be selected within provider")
			raw, err := common.GetRequestBody(next)
			require.NoError(t, err)
			require.Equal(t, body, string(raw), "must not sanitize thinking or tool history")
			plain := stateContext(1, `{"input":"continue without state"}`)
			require.NoError(t, bindResponsesState(plain, store))
			require.False(t, model.HasRetryBoundary(plain.Request.Context()))
			otherUser := stateContext(2, body)
			require.ErrorContains(t, bindResponsesState(otherUser, store), "unknown or expired")
			for key := range store.entries {
				require.NotContains(t, key, "secret-thinking")
				require.NotContains(t, key, "file contents")
			}
		})
	}
}

func TestResponsesStreamAndCompactionOrigins(t *testing.T) {
	store := newMemoryResponsesStateStore(100)
	c := stateOutputContext(1, 10, "azure")
	for _, event := range []string{
		`{"type":"response.created","response":{"id":"resp_stream"}}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_stream","encrypted_content":"stream-thinking"}}`,
		`{"type":"response.completed","response":{"id":"resp_stream","output":[{"type":"reasoning","id":"rs_stream","encrypted_content":"stream-thinking"}]}}`,
	} {
		require.NoError(t, rememberResponsesState(c, []byte(event), true, store))
	}
	next := stateContext(1, `{"input":[{"type":"reasoning","encrypted_content":"stream-thinking"}]}`)
	require.NoError(t, bindResponsesState(next, store))
	require.Equal(t, "azure", model.RetryProvider(next.Request.Context()))
	require.NoError(t, rememberResponsesState(c, []byte(`{"output":[{"type":"compaction","id":"cmp_1","encrypted_content":"compressed-state"}]}`), false, store))
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/responses/compact/?mode=test"} {
		body := `{"input":[{"type":"compaction","encrypted_content":"compressed-state"}]}`
		next := stateContext(1, body)
		next.Request = httptest.NewRequest("POST", path, strings.NewReader(body))
		require.NoError(t, bindResponsesState(next, store))
		require.Equal(t, "azure", model.RetryProvider(next.Request.Context()))
	}
}

func TestResponsesReferencesAndUnconfiguredProviderPinResource(t *testing.T) {
	for _, provider := range []string{"openai", ""} {
		store := newMemoryResponsesStateStore(100)
		output := stateOutputContext(1, 10, provider)
		output.Set("key_index", 2)
		require.NoError(t, rememberResponsesState(output, []byte(`{"id":"resp_a","conversation":{"id":"conv_a"},"output":[{"type":"message","id":"msg_a"},{"type":"reasoning","encrypted_content":"thinking"}]}`), false, store))
		for _, body := range []string{`{"previous_response_id":"resp_a"}`, `{"conversation":"conv_a"}`, `{"input":[{"type":"item_reference","id":"msg_a"}]}`} {
			next := stateContext(1, body)
			require.NoError(t, bindResponsesState(next, store))
			require.Equal(t, 10, model.ResponseStateChannel(next.Request.Context()))
			require.Equal(t, 2, next.GetInt(responsesStateKeyIndex))
			require.False(t, model.RetryProviderAllows(next.Request.Context(), &model.Channel{Id: 11, Config: `{"provider":"openai"}`}))
		}
		if provider == "" {
			next := stateContext(1, `{"input":[{"type":"reasoning","encrypted_content":"thinking"}]}`)
			require.NoError(t, bindResponsesState(next, store))
			require.Equal(t, 10, model.ResponseStateChannel(next.Request.Context()))
		}
	}
}

func TestResponsesUnknownAndConflictingOrigins(t *testing.T) {
	store := newMemoryResponsesStateStore(100)
	for _, provider := range []string{"openai", "azure"} {
		require.NoError(t, rememberResponsesState(stateOutputContext(1, 10, provider), []byte(`{"output":[{"type":"reasoning","encrypted_content":"`+provider+`"}]}`), false, store))
	}
	unknown := stateContext(1, `{"input":[{"type":"compaction","encrypted_content":"unknown"}]}`)
	require.ErrorContains(t, bindResponsesState(unknown, store), "unknown or expired")
	unknown.Request.Header.Set("X-Linkinfra-Provider", "AZURE")
	require.NoError(t, bindResponsesState(unknown, store))
	require.Equal(t, "azure", model.RetryProvider(unknown.Request.Context()))
	known := stateContext(1, `{"input":[{"type":"reasoning","encrypted_content":"openai"}]}`)
	known.Request.Header.Set("X-Linkinfra-Provider", "azure")
	require.ErrorContains(t, bindResponsesState(known, store), "incompatible providers")
	mixed := stateContext(1, `{"input":[{"type":"reasoning","encrypted_content":"openai"},{"type":"compaction","encrypted_content":"azure"}]}`)
	require.ErrorContains(t, bindResponsesState(mixed, store), "incompatible providers")
	reference := stateContext(1, `{"previous_response_id":"unknown"}`)
	reference.Request.Header.Set("X-Linkinfra-Provider", "openai")
	require.Error(t, bindResponsesState(reference, store), "a provider header cannot locate an unknown server-side reference")
}

type failingResponsesStateStore struct{}

func (failingResponsesStateStore) Lookup(context.Context, []string) (map[string]responsesStateOrigin, error) {
	return nil, errors.New("cache unavailable")
}
func (failingResponsesStateStore) Remember(context.Context, map[string]responsesStateOrigin) error {
	return errors.New("cache unavailable")
}

func TestResponsesStateCacheFailureAndExpiry(t *testing.T) {
	c := stateContext(1, `{"input":[{"type":"reasoning","encrypted_content":"state"}]}`)
	require.ErrorContains(t, bindResponsesState(c, failingResponsesStateStore{}), "cache unavailable")
	plain := stateContext(1, `{"input":"hello"}`)
	require.NoError(t, bindResponsesState(plain, failingResponsesStateStore{}), "plain requests need no state lookup")
	require.Error(t, rememberResponsesState(stateOutputContext(1, 10, "openai"), []byte(`{"id":"resp_a"}`), false, failingResponsesStateStore{}))
	store := newMemoryResponsesStateStore(2)
	now := time.Now()
	store.now = func() time.Time { return now }
	require.NoError(t, rememberResponsesState(stateOutputContext(1, 10, "openai"), []byte(`{"output":[{"type":"reasoning","encrypted_content":"state"}]}`), false, store))
	now = now.Add(responsesStateTTL + time.Second)
	require.ErrorContains(t, bindResponsesState(stateContext(1, `{"input":[{"type":"reasoning","encrypted_content":"state"}]}`), store), "unknown or expired")
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Remember(context.Background(), map[string]responsesStateOrigin{key: {ChannelID: 10}}))
	}
	require.LessOrEqual(t, len(store.entries), 2)
}

func TestResponsesMemoryStoreConcurrentAndBounded(t *testing.T) {
	store := newMemoryResponsesStateStore(100)
	var workers sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 30; i++ {
				key := fmt.Sprintf("%d:%d", worker, i)
				if err := store.Remember(context.Background(), map[string]responsesStateOrigin{key: {Provider: "openai", ChannelID: 1}}); err != nil {
					t.Error(err)
				}
				if _, err := store.Lookup(context.Background(), []string{key}); err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	workers.Wait()
	require.LessOrEqual(t, len(store.entries), 100)
	require.Equal(t, len(store.entries), store.order.Len())
}

func TestResponsesReferenceDoesNotReuseRotatedKey(t *testing.T) {
	store := newMemoryResponsesStateStore(100)
	origin := stateOutputContext(1, 10, "openai")
	origin.Set("actual_key", "original-key")
	require.NoError(t, rememberResponsesState(origin, []byte(`{"id":"resp_key"}`), false, store))
	next := stateContext(1, `{"previous_response_id":"resp_key"}`)
	require.NoError(t, bindResponsesState(next, store))
	channel := &model.Channel{Id: 10, Key: "original-key"}
	require.NoError(t, RestoreResponsesStateKey(next, channel))
	next.Set("key_index", 0)
	next.Set("actual_key", "original-key")
	require.NoError(t, VerifyResponsesStateKey(next))
	channel.Key = "rotated-key"
	require.Error(t, RestoreResponsesStateKey(next, channel))
	next.Set("actual_key", "rotated-key")
	require.Error(t, VerifyResponsesStateKey(next))
}

func TestResponsesReasoningIDWithoutCiphertextPinsResource(t *testing.T) {
	store := newMemoryResponsesStateStore(100)
	require.NoError(t, rememberResponsesState(stateOutputContext(1, 10, "azure"), []byte(`{"output":[{"type":"reasoning","id":"rs_reference","summary":[]}]}`), false, store))
	next := stateContext(1, `{"input":[{"type":"reasoning","id":"rs_reference","summary":[]}]}`)
	require.NoError(t, bindResponsesState(next, store))
	require.Equal(t, 10, model.ResponseStateChannel(next.Request.Context()))
	unknown := stateContext(1, `{"input":[{"type":"reasoning","id":"rs_unknown"}]}`)
	unknown.Request.Header.Set("X-Linkinfra-Provider", "azure")
	require.Error(t, bindResponsesState(unknown, store))
}
