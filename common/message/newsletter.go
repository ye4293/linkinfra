package message

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common/config"
)

const NewsletterSegmentOption = "ResendNewsletterSegmentId"

// Credentials are snapshotted under the same lock used by option updates.
type NewsletterClient struct {
	apiKey    string
	SegmentID string
}

func NewNewsletterClient() *NewsletterClient {
	config.OptionMapRWMutex.RLock()
	defer config.OptionMapRWMutex.RUnlock()
	return &NewsletterClient{apiKey: config.ResendApiKey, SegmentID: config.OptionMap[NewsletterSegmentOption]}
}

func (client *NewsletterClient) Configured() bool {
	return client.apiKey != "" && client.SegmentID != ""
}

// Contact synchronization stays below Resend's default two requests/second.
// 429s (including traffic from transactional mail) are retried by the durable queue.
var newsletterRequestMu sync.Mutex
var newsletterLastRequest time.Time
var newsletterRequestInterval = 600 * time.Millisecond

type NewsletterAPIError struct{ Status int }

func (err *NewsletterAPIError) Error() string { return fmt.Sprintf("resend_http_%d", err.Status) }

func (client *NewsletterClient) request(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	if client.apiKey == "" {
		return fmt.Errorf("resend_not_configured")
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	newsletterRequestMu.Lock()
	delay := time.Until(newsletterLastRequest.Add(newsletterRequestInterval))
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			newsletterRequestMu.Unlock()
			return ctx.Err()
		case <-timer.C:
		}
	}
	newsletterLastRequest = time.Now()
	newsletterRequestMu.Unlock()
	req, err := http.NewRequestWithContext(ctx, method, resendBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+client.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := resendHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &NewsletterAPIError{Status: resp.StatusCode}
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(result); err != nil {
			return fmt.Errorf("resend_invalid_response")
		}
	}
	return nil
}

type NewsletterContact struct {
	ID           string `json:"id"`
	Unsubscribed bool   `json:"unsubscribed"`
}

func (client *NewsletterClient) SyncContact(ctx context.Context, email string) (NewsletterContact, error) {
	var contact NewsletterContact
	if !client.Configured() {
		return contact, fmt.Errorf("resend_not_configured")
	}
	path := "/contacts/" + url.PathEscape(email)
	err := client.request(ctx, http.MethodGet, path, nil, &contact)
	if apiErr, ok := err.(*NewsletterAPIError); ok && apiErr.Status == http.StatusNotFound {
		// Never send unsubscribed:false: retries must not restore an opt-out.
		err = client.request(ctx, http.MethodPost, "/contacts", map[string]interface{}{
			"email": email,
		}, &contact)
		if err != nil {
			return contact, err
		}
		// Read the authoritative status, including create races with an existing contact.
		err = client.request(ctx, http.MethodGet, path, nil, &contact)
	}
	if err != nil {
		return contact, err
	}
	if contact.ID == "" {
		return contact, fmt.Errorf("resend_invalid_response")
	}
	if contact.Unsubscribed {
		return contact, nil
	}
	// A previous attempt may already have added the segment before our DB write
	// failed. Check membership so a retry never depends on duplicate-add behavior.
	member, err := client.contactInSegment(ctx, contact.ID)
	if err != nil || member {
		return contact, err
	}
	err = client.request(ctx, http.MethodPost, "/contacts/"+url.PathEscape(contact.ID)+"/segments/"+url.PathEscape(client.SegmentID), nil, nil)
	return contact, err
}

func (client *NewsletterClient) contactInSegment(ctx context.Context, contactID string) (bool, error) {
	after := ""
	for {
		path := "/contacts/" + url.PathEscape(contactID) + "/segments?limit=100"
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if err := client.request(ctx, http.MethodGet, path, nil, &result); err != nil {
			return false, err
		}
		if result.Data == nil {
			return false, fmt.Errorf("resend_invalid_response")
		}
		for _, segment := range result.Data {
			if segment.ID == client.SegmentID {
				return true, nil
			}
		}
		if !result.HasMore {
			return false, nil
		}
		if len(result.Data) == 0 || result.Data[len(result.Data)-1].ID == after {
			return false, fmt.Errorf("resend_invalid_response")
		}
		after = result.Data[len(result.Data)-1].ID
	}
}

func (client *NewsletterClient) CreateSegment(ctx context.Context, name string) (string, error) {
	var result struct {
		ID string `json:"id"`
	}
	err := client.request(ctx, http.MethodPost, "/segments", map[string]string{"name": strings.TrimSpace(name)}, &result)
	if err == nil && result.ID == "" {
		err = fmt.Errorf("resend_invalid_response")
	}
	return result.ID, err
}

func (client *NewsletterClient) CheckSegment(ctx context.Context, id string) error {
	var result struct {
		ID string `json:"id"`
	}
	if err := client.request(ctx, http.MethodGet, "/segments/"+url.PathEscape(id), nil, &result); err != nil {
		return err
	}
	if result.ID != id {
		return fmt.Errorf("resend_invalid_response")
	}
	return nil
}
