package common

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsumeVerificationCodeConcurrentReplay(t *testing.T) {
	const email = "concurrent@example.com"
	RegisterVerificationCodeWithKey(email, "abcdef", EmailVerificationPurpose)
	t.Cleanup(func() { DeleteKey(email, EmailVerificationPurpose) })
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ConsumeVerificationCodeWithKey(email, "abcdef", EmailVerificationPurpose) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := successes.Load(); n != 1 {
		t.Fatalf("successful consumers = %d, want 1", n)
	}
}

func TestConsumeVerificationCodeExpiryAndAttempts(t *testing.T) {
	const email = "expiry@example.com"
	t.Cleanup(func() { DeleteKey(email, EmailVerificationPurpose) })
	RegisterVerificationCodeWithKey(email, "abcdef", EmailVerificationPurpose)
	verificationMutex.Lock()
	value := verificationMap[EmailVerificationPurpose+email]
	value.time = time.Now().Add(-time.Duration(VerificationValidMinutes) * time.Minute)
	verificationMap[EmailVerificationPurpose+email] = value
	verificationMutex.Unlock()
	if ConsumeVerificationCodeWithKey(email, "abcdef", EmailVerificationPurpose) {
		t.Fatal("expired code accepted")
	}
	RegisterVerificationCodeWithKey(email, "abcdef", EmailVerificationPurpose)
	if ConsumeVerificationCodeWithKey("another@example.com", "abcdef", EmailVerificationPurpose) ||
		ConsumeVerificationCodeWithKey(email, "abcdef", PasswordResetPurpose) {
		t.Fatal("code accepted for another email or purpose")
	}
	for i := 0; i < 5; i++ {
		if ConsumeVerificationCodeWithKey(email, "wrong", EmailVerificationPurpose) {
			t.Fatal("wrong code accepted")
		}
	}
	if ConsumeVerificationCodeWithKey(email, "abcdef", EmailVerificationPurpose) {
		t.Fatal("code accepted after five failed attempts")
	}
	RegisterVerificationCodeWithKey(email, "newcode", EmailVerificationPurpose)
	if !ConsumeVerificationCodeWithKey(email, "newcode", EmailVerificationPurpose) {
		t.Fatal("fresh code rejected")
	}
}
