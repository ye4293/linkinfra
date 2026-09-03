package controller

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

func TestValidateOptionUpdate_EmailVerificationRequiresResend(t *testing.T) {
	oldKey, oldFrom := config.ResendApiKey, config.ResendFrom
	t.Cleanup(func() { config.ResendApiKey, config.ResendFrom = oldKey, oldFrom })

	config.ResendApiKey, config.ResendFrom = "", ""
	if msg := validateOptionUpdate(model.Option{Key: "EmailVerificationEnabled", Value: "true"}); msg == "" {
		t.Fatal("expected validation error when Resend is not configured")
	}

	config.ResendApiKey, config.ResendFrom = "re_key", "noreply@example.com"
	if msg := validateOptionUpdate(model.Option{Key: "EmailVerificationEnabled", Value: "true"}); msg != "" {
		t.Fatalf("expected no error when Resend is configured, got %q", msg)
	}

	config.ResendApiKey, config.ResendFrom = "", ""
	if msg := validateOptionUpdate(model.Option{Key: "EmailVerificationEnabled", Value: "false"}); msg != "" {
		t.Fatalf("disabling should never require Resend, got %q", msg)
	}
}
