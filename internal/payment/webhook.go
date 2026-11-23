package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Future webhook security design (no live endpoint is enabled in Phase 08):
// verify the raw body with HMAC-SHA256 over "t + "." + rawBody", using a
// constant-time comparison, reject deliveries older than five minutes,
// deduplicate X-Xendly-Delivery under a unique database constraint, and match
// event identity, order reference, amount, currency and environment before any
// idempotent state transition. Unknown event types must be acknowledged without
// action. A reconciliation job must poll GET /v1/payments/{reference} for
// missed callbacks and must use the same transition function as webhooks.
const WebhookFreshnessWindow = 5 * time.Minute

var (
	ErrInvalidWebhookSignature = errors.New("invalid webhook signature")
	ErrStaleWebhook            = errors.New("stale webhook timestamp")
)

// VerifyWebhookSignature validates a documented X-Xendly-Signature header.
// rawBody must be the exact bytes received, before any JSON parsing.
func VerifyWebhookSignature(rawBody []byte, header string, secret []byte, now time.Time) error {
	parts := strings.Split(strings.TrimSpace(header), ",")
	var timestampRaw, signatureRaw string
	for _, part := range parts {
		keyValue := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(keyValue) != 2 {
			continue
		}
		switch keyValue[0] {
		case "t":
			timestampRaw = keyValue[1]
		case "v1":
			signatureRaw = keyValue[1]
		}
	}
	if timestampRaw == "" || signatureRaw == "" || len(secret) == 0 {
		return ErrInvalidWebhookSignature
	}
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil || timestamp <= 0 {
		return ErrInvalidWebhookSignature
	}
	age := now.Sub(time.Unix(timestamp, 0).UTC())
	if age < -WebhookFreshnessWindow || age > WebhookFreshnessWindow {
		return ErrStaleWebhook
	}
	expectedMAC := hmac.New(sha256.New, secret)
	_, _ = fmt.Fprintf(expectedMAC, "%s.", timestampRaw)
	_, _ = expectedMAC.Write(rawBody)
	expected := expectedMAC.Sum(nil)
	provided, err := hex.DecodeString(signatureRaw)
	if err != nil || !hmac.Equal(expected, provided) {
		return ErrInvalidWebhookSignature
	}
	return nil
}
