package artifact

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactSignatureRejectsTampering(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(path, []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	signature, err := SignFile(path, "release-v1", private)
	if err != nil || VerifyFile(path, signature, public) != nil {
		t.Fatal("valid signature rejected")
	}
	if err = os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if VerifyFile(path, signature, public) == nil {
		t.Fatal("tampered artifact accepted")
	}
}
