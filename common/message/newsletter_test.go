package message

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newsletterServer(t *testing.T, handler http.HandlerFunc) *NewsletterClient {
	t.Helper()
	server := httptest.NewServer(handler)
	oldURL, oldInterval, oldLast := resendBaseURL, newsletterRequestInterval, newsletterLastRequest
	resendBaseURL, newsletterRequestInterval, newsletterLastRequest = server.URL, 0, time.Time{}
	t.Cleanup(func() {
		server.Close()
		resendBaseURL, newsletterRequestInterval, newsletterLastRequest = oldURL, oldInterval, oldLast
	})
	return &NewsletterClient{apiKey: "test-only", SegmentID: "segment-1"}
}

func TestNewsletterSyncCreatesContactWithoutResettingOptOut(t *testing.T) {
	calls := 0
	client := newsletterServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "Bearer test-only", r.Header.Get("Authorization"))
		switch calls {
		case 1:
			require.Equal(t, "/contacts/reader@example.com", r.URL.Path)
			w.WriteHeader(404)
		case 2:
			require.Equal(t, "POST", r.Method)
			require.Equal(t, "/contacts", r.URL.Path)
			var payload map[string]interface{}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			require.Equal(t, "reader@example.com", payload["email"])
			require.NotContains(t, payload, "unsubscribed")
			require.NotContains(t, payload, "segments")
			_, _ = w.Write([]byte(`{"id":"contact-1"}`))
		case 3:
			require.Equal(t, "GET", r.Method)
			_, _ = w.Write([]byte(`{"id":"contact-1","unsubscribed":false}`))
		case 4:
			require.Equal(t, "GET", r.Method)
			require.Equal(t, "/contacts/contact-1/segments", r.URL.Path)
			_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
		case 5:
			require.Equal(t, "POST", r.Method)
			require.Equal(t, "/contacts/contact-1/segments/segment-1", r.URL.Path)
			_, _ = w.Write([]byte(`{"id":"contact-1"}`))
		default:
			t.Errorf("unexpected request")
		}
	})
	contact, err := client.SyncContact(context.Background(), "reader@example.com")
	require.NoError(t, err)
	require.Equal(t, "contact-1", contact.ID)
	require.Equal(t, 5, calls)
}

func TestNewsletterSyncPreservesExistingUnsubscribe(t *testing.T) {
	calls := 0
	client := newsletterServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "GET", r.Method)
		_, _ = w.Write([]byte(`{"id":"contact-1","unsubscribed":true}`))
	})
	contact, err := client.SyncContact(context.Background(), "reader@example.com")
	require.NoError(t, err)
	require.True(t, contact.Unsubscribed)
	require.Equal(t, 1, calls)
}

func TestNewsletterSyncRetriesExistingContactWithoutRecreating(t *testing.T) {
	calls := 0
	client := newsletterServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.NotEqual(t, "/contacts", r.URL.Path)
		require.Equal(t, "GET", r.Method)
		if calls == 2 {
			_, _ = w.Write([]byte(`{"data":[{"id":"segment-1"}],"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"contact-1","unsubscribed":false}`))
	})
	_, err := client.SyncContact(context.Background(), "reader@example.com")
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

func TestNewsletterSyncDoesNotCreateContactOnUpstreamFailure(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := newsletterServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"private upstream details"}`))
			})
			_, err := client.SyncContact(context.Background(), "reader@example.com")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private")
			require.Equal(t, 1, calls)
		})
	}
}

func TestNewsletterSegmentValidation(t *testing.T) {
	client := newsletterServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			require.Equal(t, "/segments", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"segment-1"}`))
	})
	id, err := client.CreateSegment(context.Background(), "Newsletter")
	require.NoError(t, err)
	require.Equal(t, "segment-1", id)
	require.NoError(t, client.CheckSegment(context.Background(), id))
	require.Error(t, client.CheckSegment(context.Background(), "another-segment"))
}
