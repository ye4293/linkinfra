package model

import (
	"github.com/songquanpeng/one-api/common/config"
	"testing"
)

func TestLegacyOptionCannotDisableEmailVerification(t *testing.T) {
	oldMap, oldEnabled := config.OptionMap, config.EmailVerificationEnabled
	config.OptionMap = make(map[string]string)
	config.EmailVerificationEnabled = false
	t.Cleanup(func() { config.OptionMap, config.EmailVerificationEnabled = oldMap, oldEnabled })
	if err := updateOptionMap("EmailVerificationEnabled", "false"); err != nil {
		t.Fatal(err)
	}
	if !config.EmailVerificationEnabled || config.OptionMap["EmailVerificationEnabled"] != "true" {
		t.Fatal("legacy option disabled mandatory email verification")
	}
}
