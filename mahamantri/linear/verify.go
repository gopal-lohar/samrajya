package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// ValidSignature checks the HMAC-SHA256 of the raw body, hex-encoded,
// against the Linear-Signature header - per Linear's webhook spec, this
// must be computed over the exact bytes received, not a re-marshalled copy.
func ValidSignature(secret string, body []byte, header string) bool {
	if header == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

// FreshTimestamp guards against replayed deliveries - Linear-Timestamp is
// Unix milliseconds; anything more than a minute off is rejected.
func FreshTimestamp(header string) bool {
	return freshTimestampAt(header, time.Now())
}

func freshTimestampAt(header string, now time.Time) bool {
	ms, err := strconv.ParseInt(header, 10, 64)
	if err != nil {
		return false
	}
	sent := time.UnixMilli(ms)
	return now.Sub(sent).Abs() < time.Minute
}
