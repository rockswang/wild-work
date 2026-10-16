package officeace

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func mustGCM(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return gcm
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
