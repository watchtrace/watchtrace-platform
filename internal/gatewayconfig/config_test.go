package gatewayconfig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestSignedSnapshotRejectsTamperAndExpiry(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	resultPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := Snapshot{Version: 1, Environment: "dev", Region: "ap-south-1", ResultQueueURL: "https://sqs.local/results.fifo", GeneratedAt: now, ExpiresAt: now.Add(24 * time.Hour), Pools: []Pool{{ID: "customer-01", JobQueueURL: "https://sqs.local/jobs.fifo", ResultKeyID: "result-v1", ResultPublicKey: base64.StdEncoding.EncodeToString(resultPublic), SchemaMin: 1, SchemaMax: 2, Limits: Limits{120, 8 * 1024 * 1024, 2, 120}}}}
	data, err := Sign(snapshot, private)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Verify(data, public, now); err != nil || got.Environment != "dev" {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
	data[len(data)-5] ^= 1
	if _, err := Verify(data, public, now); err == nil {
		t.Fatal("tampered configuration accepted")
	}
	data, _ = Sign(snapshot, private)
	if _, err := Verify(data, public, now.Add(25*time.Hour)); err == nil {
		t.Fatal("expired configuration accepted")
	}
}
