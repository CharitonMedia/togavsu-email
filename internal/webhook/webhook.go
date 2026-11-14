// Package webhook checks the core's signed deliveries (event webhooks and system mail)
// with the core's scheme: X-Togavsu-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of
// "<t>.<body>" under the module's webhook secret>.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries the signature.
const SignatureHeader = "X-Togavsu-Signature"

// Event is what the core delivers for one event.
type Event struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	MatterID   string `json:"matter_id,omitempty"`
	OccurredAt string `json:"occurred_at"`
	Module     string `json:"module"`
}

// Sign returns the signature header value for body at time at.
func Sign(secret string, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature header against body within tolerance of now.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v1 == "" || secret == "" {
		return errors.New("webhook: malformed signature")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errors.New("webhook: signature outside the allowed clock skew")
	}
	if !hmac.Equal([]byte(Sign(secret, time.Unix(sec, 0), body)), []byte("t="+t+",v1="+v1)) {
		return errors.New("webhook: signature doesn't match")
	}
	return nil
}
