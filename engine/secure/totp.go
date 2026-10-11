package secure

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Time-based one-time passwords as authenticator apps implement them
// (RFC 6238): HMAC-SHA1, six digits, thirty-second steps.
const (
	totpDigits = 6
	totpStep   = 30
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret in the base32 form that
// authenticator apps accept.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return totpEncoding.EncodeToString(b)
}

// TOTPCode computes the code for a secret at a time.
func TOTPCode(secret string, at time.Time) (string, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("invalid authenticator secret")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(at.Unix()/totpStep))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1000000), nil
}

// VerifyTOTP checks a code against the current step and the one on either
// side, which allows for clock drift between the server and the phone.
func VerifyTOTP(secret, code string, at time.Time) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return false
	}
	ok := false
	for _, drift := range []int{-1, 0, 1} {
		want, err := TOTPCode(secret, at.Add(time.Duration(drift*totpStep)*time.Second))
		if err != nil {
			return false
		}
		// Check every window so timing does not show which one matched.
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			ok = true
		}
	}
	return ok
}

// TOTPURI is the otpauth:// address an authenticator app reads, usually
// from a QR code.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// NewRecoveryCode returns a one-time code of the form xxxxx-xxxxx for
// signing in when the authenticator is unavailable.
func NewRecoveryCode() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no look-alike characters
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:5]) + "-" + string(b[5:])
}
