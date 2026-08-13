package mtlspki

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssueCreatesThirtyDayPoolClientIdentity(t *testing.T) {
	now := time.Now().UTC()
	root, key, err := NewRoot(now)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, serial, err := Issue(root, key, "pool-a", now, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(cert)
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil || parsed.Subject.CommonName != "pool-a" || serial != parsed.SerialNumber.String() || parsed.NotAfter.Sub(now) > 30*24*time.Hour+time.Second || len(parsed.ExtKeyUsage) != 1 || parsed.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("certificate=%+v err=%v", parsed, err)
	}
	if _, _, _, err = Issue(root, key, "pool-a", now, 31*24*time.Hour); err == nil {
		t.Fatal("overlong certificate accepted")
	}
}
