package envelope

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func testJob(version int, now time.Time) Job {
	return Job{SchemaVersion: version, JobID: "550e8400-e29b-41d4-a716-446655440000", JobType: "scheduled", WorkerPoolID: "hosted", NetworkPolicyVersion: 1, ScheduledAt: now, ExpiresAt: now.Add(2 * time.Minute), TargetURL: "https://controlled.example.test", Method: "GET", TimeoutSeconds: 5, ExpectedStatusMin: 200, ExpectedStatusMax: 299, Headers: map[string]string{"X-B": "2", "X-A": "1"}, Limits: RequestLimits{65536, 32768, 3}, PlatformKeyID: "platform-v1", WorkerEncryptionKeyID: "worker-v1"}
}

func TestJobSealOpenUsesCanonicalSnapshotAndFreshNonce(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	worker, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := testJob(SchemaVersion, now)
	first, attrs, err := SealJob(job, private, worker.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	second, secondAttrs, err := SealJob(job, private, worker.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("job encryption reused its ephemeral key or nonce")
	}
	if attrs != secondAttrs {
		t.Fatalf("canonical snapshot identity changed: %+v / %+v", attrs, secondAttrs)
	}
	got, err := OpenJob(first, attrs, worker, public, now, MaxClockSkew)
	if err != nil || got.JobID != job.JobID || got.Headers["X-A"] != "1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestJobRejectsTamperAssociatedDataWrongKeyAndDowngrade(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	worker, _ := ecdh.X25519().GenerateKey(rand.Reader)
	wrongWorker, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	body, attrs, err := SealJob(testJob(SchemaVersion, now), private, worker.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	changed := attrs
	changed.WorkerPoolID = "other"
	if _, err = OpenJob(body, changed, worker, public, now, 0); err == nil {
		t.Fatal("associated-data mismatch accepted")
	}
	if _, err = OpenJob(body, attrs, wrongWorker, public, now, 0); err == nil {
		t.Fatal("wrong encryption key accepted")
	}
	body[len(body)-1] ^= 1
	if _, err = OpenJob(body, attrs, worker, public, now, 0); err == nil {
		t.Fatal("tamper accepted")
	}
	attrs.SchemaVersion = 0
	if _, err = OpenJob(body, attrs, worker, public, now, 0); err == nil {
		t.Fatal("protocol downgrade accepted")
	}
}

func TestCurrentAndPreviousSchemasInteroperate(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	worker, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	for _, version := range []int{PreviousSchemaVersion, SchemaVersion} {
		body, attrs, err := SealJob(testJob(version, now), private, worker.PublicKey())
		if err != nil {
			t.Fatalf("seal version %d: %v", version, err)
		}
		if got, err := OpenJob(body, attrs, worker, public, now, 0); err != nil || got.SchemaVersion != version {
			t.Fatalf("open version %d: got=%+v err=%v", version, got, err)
		}
	}
}

func TestResultIdentityIsSignedAndDifferentAttemptsRemainDeliverable(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	status := int16(204)
	base := Result{SchemaVersion: SchemaVersion, ResultID: "550e8400-e29b-41d4-a716-446655440001", JobID: "550e8400-e29b-41d4-a716-446655440000", SnapshotHash: string(bytes.Repeat([]byte{'a'}, 64)), WorkerPoolID: "hosted", WorkerID: "worker", AttemptID: "550e8400-e29b-41d4-a716-446655440002", ScheduledAt: now, StartedAt: now, CompletedAt: now.Add(time.Second), Succeeded: true, StatusCode: &status, TotalMicros: 1_000_000, ResultKeyID: "result-v1"}
	first, err := SignResult(base, private)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyResult(first, public)
	if err != nil || got.ResultID != base.ResultID {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	secondAttempt := base
	secondAttempt.ResultID = "550e8400-e29b-41d4-a716-446655440003"
	secondAttempt.AttemptID = "550e8400-e29b-41d4-a716-446655440004"
	second, err := SignResult(secondAttempt, private)
	if err != nil || bytes.Equal(first, second) {
		t.Fatalf("second attempt not independently identified: %v", err)
	}
	first[len(first)-1] ^= 1
	if _, err = VerifyResult(first, public); err == nil {
		t.Fatal("tampered result accepted")
	}
}
