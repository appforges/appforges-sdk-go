package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// VerifyWebhookSignature verifies IAM's current raw-body protocol using the
// original secrets returned by create/rotate. It does not prevent replay or
// authenticate X-Webhook-Event. Multiple local secrets support key rotation.
func VerifyWebhookSignature(body []byte, signature string, secrets []string) bool {
	if len(signature) != 64 {
		return false
	}
	for _, c := range signature {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	actual, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	valid := false
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		digest := sha256.Sum256([]byte(secret))
		mac := hmac.New(sha256.New, []byte(hex.EncodeToString(digest[:])))
		_, _ = mac.Write(body)
		valid = hmac.Equal(mac.Sum(nil), actual) || valid
	}
	return valid
}
