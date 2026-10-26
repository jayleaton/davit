package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Signature returns the X-Davit-Signature value for body at t: one v1
// per secret (two during a rotation overlap), each
// hex(HMAC-SHA256(secret, "<unix t>.<body>")).
func Signature(t time.Time, body []byte, secrets ...[]byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	parts := []string{"t=" + ts}
	for _, s := range secrets {
		parts = append(parts, "v1="+hex.EncodeToString(mac(s, ts, body)))
	}
	return strings.Join(parts, ",")
}

func mac(secret []byte, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return m.Sum(nil)
}

// ErrSignature is a delivery that fails verification.
var ErrSignature = errors.New("webhook signature does not verify")

// Verify checks a delivery the way a receiver should: the timestamp is
// within tolerance of now, and some v1 matches some secret (constant-time).
func Verify(header string, body []byte, now time.Time, tolerance time.Duration, secrets ...[]byte) error {
	var ts string
	var sigs [][]byte
	for _, p := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		switch {
		case !ok:
			return ErrSignature
		case k == "t":
			ts = v
		case k == "v1":
			b, err := hex.DecodeString(v)
			if err != nil {
				return ErrSignature
			}
			sigs = append(sigs, b)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return ErrSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return ErrSignature
	}
	for _, s := range secrets {
		want := mac(s, ts, body)
		for _, got := range sigs {
			if hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return ErrSignature
}
