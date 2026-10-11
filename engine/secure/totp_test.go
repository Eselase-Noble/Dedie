package secure

import (
	"testing"
	"time"
)

// Test vectors from RFC 6238, appendix B (SHA-1, truncated to six digits).
func TestTOTPVectors(t *testing.T) {
	secret := totpEncoding.EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		got, err := TOTPCode(secret, time.Unix(unix, 0))
		if err != nil || got != want {
			t.Errorf("at %d: got %s (%v), want %s", unix, got, err, want)
		}
	}
}

func TestVerifyTOTP(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1700000000, 0)
	code, _ := TOTPCode(secret, now)
	if !VerifyTOTP(secret, code, now) || !VerifyTOTP(secret, code, now.Add(25*time.Second)) {
		t.Error("a current code was rejected")
	}
	if VerifyTOTP(secret, code, now.Add(5*time.Minute)) {
		t.Error("an old code was accepted")
	}
	if VerifyTOTP(secret, "12345", now) || VerifyTOTP(secret, "", now) {
		t.Error("a malformed code was accepted")
	}
}
