package deploymentkeys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateAndVerify(t *testing.T) {
	directory := t.TempDir()
	if err := Generate(directory); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := Verify(directory); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, name := range generatedFiles {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %04o, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestGenerateNeverOverwrites(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, PlatformSigningPrivate)
	if err := os.WriteFile(path, []byte("keep-this-value"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Generate(directory); err == nil {
		t.Fatal("Generate succeeded in a non-empty directory")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "keep-this-value" {
		t.Fatal("Generate replaced an existing file")
	}
}

func TestVerifyRejectsMismatchedBundle(t *testing.T) {
	directory := t.TempDir()
	if err := Generate(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, HostedPublicBundle), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(directory); err == nil {
		t.Fatal("Verify accepted a mismatched public bundle")
	}
}
