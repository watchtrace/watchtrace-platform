package modworker

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/watchtrace/watchtrace-platform/internal/checkengine"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workerjournal"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type transport struct {
	delivery      workqueue.Delivery
	published     int
	publishedBody []byte
	acked         int
	pulls         int
	failPublish   bool
}

func (t *transport) Pull(context.Context, time.Duration) (workqueue.Delivery, error) {
	t.pulls++
	return t.delivery, nil
}
func (t *transport) Extend(context.Context, workqueue.Delivery, time.Duration) error { return nil }
func (t *transport) PublishResultAndAcknowledge(_ context.Context, _ workqueue.Delivery, body []byte) error {
	t.published++
	t.publishedBody = append([]byte(nil), body...)
	if t.failPublish {
		return errors.New("result path unavailable")
	}
	t.acked++
	return nil
}

func TestWorkerStopsPullingUntilJournalPublicationRecovers(t *testing.T) {
	platformPub, platformPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, resultPriv, _ := ed25519.GenerateKey(rand.Reader)
	workerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	job := envelope.Job{SchemaVersion: 2, JobID: "job-circuit", JobType: "scheduled", WorkerPoolID: "hosted", NetworkPolicyVersion: 1, ScheduledAt: now, ExpiresAt: now.Add(time.Minute), TargetURL: "https://example.com", Method: "GET", TimeoutSeconds: 1, ExpectedStatusMin: 200, ExpectedStatusMax: 299, Limits: envelope.RequestLimits{MaxResponseBytes: 64, MaxHeaderBytes: 1024, MaxRedirects: 3}, PlatformKeyID: "p", WorkerEncryptionKeyID: "w"}
	body, attrs, err := envelope.SealJob(job, platformPriv, workerKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	tr := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}, failPublish: true}
	journal, err := workerjournal.Open(filepath.Join(t.TempDir(), "circuit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	engine := checkengine.NewWithClient(doer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: io.NopCloser(&empty{})}, nil
	}))
	worker, err := New(tr, journal, engine, Config{WorkerID: "worker", WorkerPoolID: "hosted", PlatformKeyID: "p", WorkerEncryptionKeyID: "w", ResultKeyID: "r", WorkerPrivate: workerKey, PlatformPublic: platformPub, ResultPrivate: resultPriv})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOne(context.Background()); err == nil || worker.Ready() {
		t.Fatal("result outage did not open worker circuit")
	}
	if tr.pulls != 1 {
		t.Fatalf("pulls=%d", tr.pulls)
	}
	if _, err = worker.RunOne(context.Background()); err == nil || tr.pulls != 1 {
		t.Fatal("worker pulled while result circuit was open")
	}
	tr.failPublish = false
	if _, err = worker.RunOne(context.Background()); err != nil || !worker.Ready() || tr.pulls != 1 {
		t.Fatalf("recovery err=%v ready=%t pulls=%d", err, worker.Ready(), tr.pulls)
	}
}

func TestWorkerAcceptsPreviousKeysAndRejectsEmergencyRevocation(t *testing.T) {
	oldPub, oldPriv, _ := ed25519.GenerateKey(rand.Reader)
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, resultPriv, _ := ed25519.GenerateKey(rand.Reader)
	oldWorker, _ := ecdh.X25519().GenerateKey(rand.Reader)
	newWorker, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	job := envelope.Job{SchemaVersion: 1, JobID: "old-key-job", JobType: "scheduled", WorkerPoolID: "hosted", NetworkPolicyVersion: 1, ScheduledAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute), TargetURL: "https://example.com", Method: "GET", TimeoutSeconds: 1, ExpectedStatusMin: 200, ExpectedStatusMax: 299, Limits: envelope.RequestLimits{MaxResponseBytes: 64, MaxHeaderBytes: 1024, MaxRedirects: 3}, PlatformKeyID: "platform-old", WorkerEncryptionKeyID: "worker-old"}
	body, attrs, err := envelope.SealJob(job, oldPriv, oldWorker.PublicKey())
	if err == nil {
		t.Fatal("test job unexpectedly sealed after expiry")
	}
	job.ScheduledAt = now
	job.ExpiresAt = now.Add(time.Minute)
	body, attrs, err = envelope.SealJob(job, oldPriv, oldWorker.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	makeWorker := func(revoked map[string]struct{}) (*Worker, *transport) {
		tr := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}}
		journal, _ := workerjournal.Open(filepath.Join(t.TempDir(), randomID()+".db"))
		t.Cleanup(func() { journal.Close() })
		engine := checkengine.NewWithClient(doer(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 204, Body: io.NopCloser(&empty{})}, nil
		}))
		w, err := New(tr, journal, engine, Config{WorkerID: "worker", WorkerPoolID: "hosted", ResultKeyID: "result", ResultPrivate: resultPriv, WorkerPrivateKeys: map[string]*ecdh.PrivateKey{"worker-old": oldWorker, "worker-new": newWorker}, PlatformPublicKeys: map[string]ed25519.PublicKey{"platform-old": oldPub, "platform-new": newPub}, RevokedKeyIDs: revoked})
		if err != nil {
			t.Fatal(err)
		}
		return w, tr
	}
	w, tr := makeWorker(nil)
	if _, err = w.RunOne(context.Background()); err != nil || tr.acked != 1 {
		t.Fatalf("previous key rejected: %v", err)
	}
	revoked, _ := makeWorker(map[string]struct{}{"platform-old": {}})
	if _, err = revoked.RunOne(context.Background()); !errors.Is(err, envelope.ErrInvalid) {
		t.Fatalf("revoked key accepted: %v", err)
	}
}
func (t *transport) AcknowledgeExpired(context.Context, workqueue.Delivery, []byte) error {
	t.acked++
	return nil
}

type doer func(*http.Request) (*http.Response, error)

func (f doer) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestWorkerJournalPreventsSameWorkerRepeat(t *testing.T) {
	platformPub, platformPriv, _ := ed25519.GenerateKey(rand.Reader)
	resultPub, resultPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = resultPub
	workerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	job := envelope.Job{SchemaVersion: 1, JobID: "job", JobType: "scheduled", WorkerPoolID: "hosted", NetworkPolicyVersion: 1, ScheduledAt: now, ExpiresAt: now.Add(time.Minute), TargetURL: "https://example.com", Method: "GET", TimeoutSeconds: 1, ExpectedStatusMin: 200, ExpectedStatusMax: 299, Limits: envelope.RequestLimits{MaxResponseBytes: 64, MaxHeaderBytes: 1024, MaxRedirects: 3}, PlatformKeyID: "p", WorkerEncryptionKeyID: "w"}
	body, attrs, _ := envelope.SealJob(job, platformPriv, workerKey.PublicKey())
	tr := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}}
	journal, _ := workerjournal.Open(filepath.Join(t.TempDir(), "j.db"))
	defer journal.Close()
	calls := 0
	engine := checkengine.NewWithClient(doer(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 204, Body: io.NopCloser(&empty{})}, nil
	}))
	w, _ := New(tr, journal, engine, Config{WorkerID: "worker", WorkerPoolID: "hosted", PlatformKeyID: "p", WorkerEncryptionKeyID: "w", ResultKeyID: "r", WorkerPrivate: workerKey, PlatformPublic: platformPub, ResultPrivate: resultPriv})
	if _, err := w.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || tr.published != 2 {
		t.Fatalf("calls=%d published=%d", calls, tr.published)
	}
}

func TestWorkerRestartReplaysDurableJournalBeforeReceivingNewWork(t *testing.T) {
	platformPub, platformPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, resultPriv, _ := ed25519.GenerateKey(rand.Reader)
	workerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	job := envelope.Job{SchemaVersion: 2, JobID: "restart-job", JobType: "scheduled", WorkerPoolID: "hosted", NetworkPolicyVersion: 1, ScheduledAt: now, ExpiresAt: now.Add(time.Minute), TargetURL: "https://example.com", Method: "GET", TimeoutSeconds: 1, ExpectedStatusMin: 200, ExpectedStatusMax: 299, Limits: envelope.RequestLimits{MaxResponseBytes: 64, MaxHeaderBytes: 1024, MaxRedirects: 3}, PlatformKeyID: "p", WorkerEncryptionKeyID: "w"}
	body, attrs, err := envelope.SealJob(job, platformPriv, workerKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(t.TempDir(), "restart.sqlite")
	journal, err := workerjournal.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	engine := checkengine.NewWithClient(doer(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 204, Body: io.NopCloser(&empty{})}, nil
	}))
	config := Config{WorkerID: "worker", WorkerPoolID: "hosted", PlatformKeyID: "p", WorkerEncryptionKeyID: "w", ResultKeyID: "r", WorkerPrivate: workerKey, PlatformPublic: platformPub, ResultPrivate: resultPriv}
	failed := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}, failPublish: true}
	worker, err := New(failed, journal, engine, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOne(context.Background()); err == nil {
		t.Fatal("result publication outage was not reported")
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := workerjournal.Open(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}}
	worker, err = New(recovered, reopened, engine, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || recovered.published != 1 || recovered.acked != 1 {
		t.Fatalf("calls=%d published=%d acked=%d", calls, recovered.published, recovered.acked)
	}
}

func TestLostJournalDocumentsRareDuplicateRequestWindow(t *testing.T) {
	platformPub, platformPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, resultPriv, _ := ed25519.GenerateKey(rand.Reader)
	workerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	now := time.Now().UTC()
	job := envelope.Job{SchemaVersion: 2, JobID: "lost-journal-job", JobType: "scheduled", WorkerPoolID: "hosted", NetworkPolicyVersion: 1, ScheduledAt: now, ExpiresAt: now.Add(time.Minute), TargetURL: "https://example.com", Method: "GET", TimeoutSeconds: 1, ExpectedStatusMin: 200, ExpectedStatusMax: 299, Limits: envelope.RequestLimits{MaxResponseBytes: 64, MaxHeaderBytes: 1024, MaxRedirects: 3}, PlatformKeyID: "p", WorkerEncryptionKeyID: "w"}
	body, attrs, err := envelope.SealJob(job, platformPriv, workerKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	engine := checkengine.NewWithClient(doer(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 204, Body: io.NopCloser(&empty{})}, nil
	}))
	config := Config{WorkerID: "worker", WorkerPoolID: "hosted", PlatformKeyID: "p", WorkerEncryptionKeyID: "w", ResultKeyID: "r", WorkerPrivate: workerKey, PlatformPublic: platformPub, ResultPrivate: resultPriv}
	path := filepath.Join(t.TempDir(), "lost.sqlite")
	firstJournal, _ := workerjournal.Open(path)
	firstTransport := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}, failPublish: true}
	first, _ := New(firstTransport, firstJournal, engine, config)
	if _, err = first.RunOne(context.Background()); err == nil {
		t.Fatal("ambiguous publication was not reported")
	}
	firstResult, err := envelope.PeekResult(first.pending.body)
	if err != nil {
		t.Fatal(err)
	}
	_ = firstJournal.Close()
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	secondJournal, _ := workerjournal.Open(path)
	defer secondJournal.Close()
	secondTransport := &transport{delivery: workqueue.Delivery{Body: body, Attributes: attrs}}
	second, _ := New(secondTransport, secondJournal, engine, config)
	if _, err = second.RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondResult, err := envelope.PeekResult(secondTransport.deliveryResult())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || firstResult.ResultID == secondResult.ResultID || firstResult.AttemptID == secondResult.AttemptID {
		t.Fatalf("calls=%d first=%s second=%s", calls, firstResult.ResultID, secondResult.ResultID)
	}
}

func (t *transport) deliveryResult() []byte {
	if t.publishedBody == nil {
		return nil
	}
	return t.publishedBody
}

type empty struct{}

func (*empty) Read([]byte) (int, error) { return 0, io.EOF }
func (*empty) Close() error             { return nil }
