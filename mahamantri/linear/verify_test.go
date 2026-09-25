package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"action":"create"}`)
	secret := "s3cret"
	sig := sign(secret, body)

	if !ValidSignature(secret, body, sig) {
		t.Error("correct signature was rejected")
	}
	if ValidSignature(secret, []byte(`{"action":"tampered"}`), sig) {
		t.Error("tampered body was accepted")
	}
	if ValidSignature(secret, body, "") {
		t.Error("missing signature was accepted")
	}
	if ValidSignature(secret, body, "not-a-real-signature") {
		t.Error("garbage signature was accepted")
	}
}

func TestFreshTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	fresh := strconv.FormatInt(now.Add(-30*time.Second).UnixMilli(), 10)
	stale := strconv.FormatInt(now.Add(-2*time.Minute).UnixMilli(), 10)
	future := strconv.FormatInt(now.Add(2*time.Minute).UnixMilli(), 10)

	if !freshTimestampAt(fresh, now) {
		t.Error("30s-old timestamp should be fresh")
	}
	if freshTimestampAt(stale, now) {
		t.Error("2min-old timestamp should be stale")
	}
	if freshTimestampAt(future, now) {
		t.Error("2min-in-the-future timestamp should be rejected")
	}
	if freshTimestampAt("not-a-number", now) {
		t.Error("garbage timestamp should be rejected")
	}
}
