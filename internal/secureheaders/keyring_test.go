package secureheaders

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncryptedHeadersRoundTripAndRedact(t *testing.T) {
	ring, err := New(2, map[int32][]byte{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	first, version, err := ring.Encrypt(map[string]string{"authorization": "Bearer secret", "x-probe": "yes"})
	if err != nil || version != 2 {
		t.Fatalf("encrypt version=%d err=%v", version, err)
	}
	second, _, _ := ring.Encrypt(map[string]string{"authorization": "Bearer secret", "x-probe": "yes"})
	if bytes.Equal(first, second) || strings.Contains(string(first), "secret") {
		t.Fatal("encryption is deterministic or leaked plaintext")
	}
	got, err := ring.Decrypt(first, version)
	if err != nil || got["Authorization"] != "Bearer secret" {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestEncryptedHeadersRejectUnsafeValues(t *testing.T) {
	ring, _ := New(1, map[int32][]byte{1: bytes.Repeat([]byte{1}, 32)})
	for _, input := range []map[string]string{{"Host": "evil"}, {"X-Test": "a\r\nb"}} {
		if _, _, err := ring.Encrypt(input); !errorsIsInvalid(err) {
			t.Fatalf("error=%v", err)
		}
	}
}

func errorsIsInvalid(err error) bool { return err == ErrInvalid }
