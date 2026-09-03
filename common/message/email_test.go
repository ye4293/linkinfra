package message

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

type capturedRequest struct {
	Authorization string
	Path          string
	Body          map[string]any
}

func setupResendServer(t *testing.T, status int, respBody string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Authorization = r.Header.Get("Authorization")
		captured.Path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(server.Close)

	oldBase, oldKey, oldFrom, oldName := resendBaseURL, config.ResendApiKey, config.ResendFrom, config.SystemName
	resendBaseURL = server.URL
	config.ResendApiKey = "re_test_key"
	config.ResendFrom = "noreply@example.com"
	config.SystemName = "LinkInfra"
	t.Cleanup(func() {
		resendBaseURL, config.ResendApiKey, config.ResendFrom, config.SystemName = oldBase, oldKey, oldFrom, oldName
	})
	return server, captured
}

func TestSendEmail_PostsToResendWithExpectedPayload(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)

	err := SendEmail("Verification code", "a@example.com;b@example.com", "<p>hi</p>")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if captured.Path != "/emails" {
		t.Errorf("expected path /emails, got %s", captured.Path)
	}
	if captured.Authorization != "Bearer re_test_key" {
		t.Errorf("unexpected authorization header: %q", captured.Authorization)
	}
	if got := captured.Body["from"]; got != "LinkInfra <noreply@example.com>" {
		t.Errorf("unexpected from: %v", got)
	}
	if got := captured.Body["subject"]; got != "[LinkInfra] Verification code" {
		t.Errorf("unexpected subject: %v", got)
	}
	if got := captured.Body["html"]; got != "<p>hi</p>" {
		t.Errorf("unexpected html: %v", got)
	}
	to, _ := captured.Body["to"].([]any)
	if len(to) != 2 || to[0] != "a@example.com" || to[1] != "b@example.com" {
		t.Errorf("unexpected to: %v", captured.Body["to"])
	}
}

func TestSendEmail_ReturnsErrorWhenNotConfigured(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)
	config.ResendApiKey = ""

	err := SendEmail("subject", "a@example.com", "body")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected not configured error, got %v", err)
	}
	if captured.Path != "" {
		t.Errorf("expected no request to be sent, got path %s", captured.Path)
	}
}

func TestSendEmail_ReturnsResendErrorMessageOnFailure(t *testing.T) {
	setupResendServer(t, http.StatusForbidden, `{"statusCode":403,"name":"validation_error","message":"Domain is not verified"}`)

	err := SendEmail("subject", "a@example.com", "body")
	if err == nil || !strings.Contains(err.Error(), "Domain is not verified") {
		t.Fatalf("expected resend error message, got %v", err)
	}
}

func TestSendEmail_RejectsEmptyReceiver(t *testing.T) {
	setupResendServer(t, http.StatusOK, `{"id":"abc"}`)

	err := SendEmail("subject", "", "body")
	if err == nil {
		t.Fatal("expected error for empty receiver")
	}
}

func TestSendEmail_KeepsExistingDisplayNameInFrom(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)
	config.ResendFrom = "Support <noreply@example.com>"

	if err := SendEmail("subject", "a@example.com", "body"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got := captured.Body["from"]; got != "Support <noreply@example.com>" {
		t.Errorf("expected from to be passed through unchanged, got %v", got)
	}
}

func TestSendEmail_QuotesSystemNameWithSpecialCharacters(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)
	config.SystemName = `Foo, "Inc."`

	if err := SendEmail("subject", "a@example.com", "body"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got := captured.Body["from"]; got != `"Foo, \"Inc.\"" <noreply@example.com>` {
		t.Errorf("unexpected from: %v", got)
	}
}

func TestSendEmail_UsesBareFromAndSubjectWhenSystemNameEmpty(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)
	config.SystemName = ""

	if err := SendEmail("subject", "a@example.com", "body"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got := captured.Body["from"]; got != "noreply@example.com" {
		t.Errorf("unexpected from: %v", got)
	}
	if got := captured.Body["subject"]; got != "subject" {
		t.Errorf("unexpected subject: %v", got)
	}
}

func TestSendEmail_ReturnsErrorWhenFromNotConfigured(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)
	config.ResendFrom = ""

	err := SendEmail("subject", "a@example.com", "body")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected not configured error, got %v", err)
	}
	if captured.Path != "" {
		t.Errorf("expected no request to be sent, got path %s", captured.Path)
	}
}

func TestSendEmail_ReturnsStatusAndBodyOnNonJSONError(t *testing.T) {
	setupResendServer(t, http.StatusBadGateway, `<html>bad gateway</html>`)

	err := SendEmail("subject", "a@example.com", "body")
	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("expected status and body in error, got %v", err)
	}
}

func TestSendEmail_RejectsReceiverWithOnlySeparators(t *testing.T) {
	_, captured := setupResendServer(t, http.StatusOK, `{"id":"abc"}`)

	err := SendEmail("subject", " ; ", "body")
	if err == nil {
		t.Fatal("expected error for receiver with only separators")
	}
	if captured.Path != "" {
		t.Errorf("expected no request to be sent, got path %s", captured.Path)
	}
}
