package quarantine

import (
	"bytes"
	"testing"
)

func TestSealerBindsAssociatedDataAndUsesFreshNonces(t *testing.T) {
	sealer, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	first, err := sealer.Seal([]byte("signed-result"), []byte("result:one"))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := sealer.Seal([]byte("signed-result"), []byte("result:one"))
	if bytes.Equal(first, second) {
		t.Fatal("quarantine encryption reused a nonce")
	}
	if _, err = sealer.Open(first, []byte("result:two")); err == nil {
		t.Fatal("associated-data mismatch was accepted")
	}
	plain, err := sealer.Open(first, []byte("result:one"))
	if err != nil || string(plain) != "signed-result" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}
