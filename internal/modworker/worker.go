// Package modworker implements the database-free modular check worker.
package modworker

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/checkengine"
	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workerjournal"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

type Config struct {
	WorkerID, WorkerPoolID, PlatformKeyID, WorkerEncryptionKeyID, ResultKeyID string
	ClockTolerance                                                            time.Duration
	WorkerPrivate                                                             *ecdh.PrivateKey
	PlatformPublic                                                            ed25519.PublicKey
	ResultPrivate                                                             ed25519.PrivateKey
	WorkerPrivateKeys                                                         map[string]*ecdh.PrivateKey
	PlatformPublicKeys                                                        map[string]ed25519.PublicKey
	RevokedKeyIDs                                                             map[string]struct{}
}
type Worker struct {
	transport workqueue.Transport
	journal   *workerjournal.Journal
	engine    *checkengine.Engine
	config    Config
	now       func() time.Time
	pending   *pendingPublication
}

type pendingPublication struct {
	delivery workqueue.Delivery
	body     []byte
	expired  bool
}

func New(t workqueue.Transport, j *workerjournal.Journal, e *checkengine.Engine, c Config) (*Worker, error) {
	if c.WorkerPrivateKeys == nil {
		c.WorkerPrivateKeys = map[string]*ecdh.PrivateKey{}
	}
	if c.PlatformPublicKeys == nil {
		c.PlatformPublicKeys = map[string]ed25519.PublicKey{}
	}
	if c.WorkerPrivate != nil && c.WorkerEncryptionKeyID != "" {
		c.WorkerPrivateKeys[c.WorkerEncryptionKeyID] = c.WorkerPrivate
	}
	if len(c.PlatformPublic) == ed25519.PublicKeySize && c.PlatformKeyID != "" {
		c.PlatformPublicKeys[c.PlatformKeyID] = c.PlatformPublic
	}
	if t == nil || j == nil || e == nil || c.WorkerID == "" || c.WorkerPoolID == "" || c.ResultKeyID == "" || len(c.WorkerPrivateKeys) == 0 || len(c.PlatformPublicKeys) == 0 || len(c.ResultPrivate) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid worker configuration")
	}
	if c.ClockTolerance < 0 || c.ClockTolerance > 30*time.Second {
		return nil, errors.New("invalid clock tolerance")
	}
	return &Worker{transport: t, journal: j, engine: e, config: c, now: time.Now}, nil
}
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	if w.pending != nil {
		pending := w.pending
		var err error
		if pending.expired {
			err = w.transport.AcknowledgeExpired(ctx, pending.delivery, pending.body)
		} else {
			err = w.transport.PublishResultAndAcknowledge(ctx, pending.delivery, pending.body)
		}
		if err != nil {
			return true, err
		}
		w.pending = nil
		return true, nil
	}
	delivery, err := w.transport.Pull(ctx, 20*time.Second)
	if errors.Is(err, workqueue.ErrNoMessage) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, revoked := w.config.RevokedKeyIDs[delivery.Attributes.PlatformKeyID]; revoked {
		return true, envelope.ErrInvalid
	}
	if _, revoked := w.config.RevokedKeyIDs[delivery.Attributes.WorkerEncryptionKeyID]; revoked {
		return true, envelope.ErrInvalid
	}
	workerPrivate, workerOK := w.config.WorkerPrivateKeys[delivery.Attributes.WorkerEncryptionKeyID]
	platformPublic, platformOK := w.config.PlatformPublicKeys[delivery.Attributes.PlatformKeyID]
	if !workerOK || !platformOK {
		return true, envelope.ErrInvalid
	}
	job, err := envelope.OpenJob(delivery.Body, delivery.Attributes, workerPrivate, platformPublic, w.now().UTC(), w.config.ClockTolerance)
	if err != nil {
		return true, err
	}
	if job.WorkerPoolID != w.config.WorkerPoolID {
		return true, envelope.ErrInvalid
	}
	if stored, ok, err := w.journal.Result(ctx, job.JobID, delivery.Attributes.SnapshotHash); err != nil {
		return true, err
	} else if ok {
		if err = w.transport.PublishResultAndAcknowledge(ctx, delivery, stored); err != nil {
			w.pending = &pendingPublication{delivery: delivery, body: append([]byte(nil), stored...)}
			return true, err
		}
		return true, nil
	}
	if w.now().UTC().After(job.ExpiresAt.Add(w.config.ClockTolerance)) {
		ack, err := envelope.SignExpired(envelope.ExpiredAcknowledgement{SchemaVersion: job.SchemaVersion, JobID: job.JobID, SnapshotHash: delivery.Attributes.SnapshotHash, WorkerPoolID: job.WorkerPoolID, WorkerID: w.config.WorkerID, ExpiredAt: w.now().UTC(), ResultKeyID: w.config.ResultKeyID}, w.config.ResultPrivate)
		if err != nil {
			return true, err
		}
		if err = w.transport.AcknowledgeExpired(ctx, delivery, ack); err != nil {
			w.pending = &pendingPublication{delivery: delivery, body: append([]byte(nil), ack...), expired: true}
			return true, err
		}
		return true, nil
	}
	if err = w.journal.Accept(ctx, job.JobID, delivery.Attributes.SnapshotHash); err != nil {
		return true, err
	}
	attempt := randomID()
	resultID := randomID()
	result, err := w.engine.Execute(ctx, checkengine.Request{JobID: job.JobID, URL: job.TargetURL, Method: job.Method, Headers: job.Headers, Timeout: time.Duration(job.TimeoutSeconds) * time.Second, ExpectedMin: int(job.ExpectedStatusMin), ExpectedMax: int(job.ExpectedStatusMax), MaxResponseBytes: job.Limits.MaxResponseBytes})
	if err != nil {
		return true, err
	}
	signed, err := envelope.SignResult(envelope.Result{SchemaVersion: job.SchemaVersion, ResultID: resultID, JobID: job.JobID, SnapshotHash: delivery.Attributes.SnapshotHash, WorkerPoolID: job.WorkerPoolID, WorkerID: w.config.WorkerID, AttemptID: attempt, ScheduledAt: job.ScheduledAt, StartedAt: result.StartedAt, CompletedAt: result.CompletedAt, Succeeded: result.Succeeded, StatusCode: result.StatusCode, ErrorCategory: result.ErrorCategory, DNSMicros: micros(result.DNS), ConnectMicros: micros(result.Connect), TLSMicros: micros(result.TLS), FirstByteMicros: micros(result.FirstByte), TotalMicros: result.Total.Microseconds(), ResultKeyID: w.config.ResultKeyID}, w.config.ResultPrivate)
	if err != nil {
		return true, err
	}
	if err = w.journal.StoreResult(ctx, job.JobID, delivery.Attributes.SnapshotHash, signed); err != nil {
		return true, err
	}
	if err = w.transport.PublishResultAndAcknowledge(ctx, delivery, signed); err != nil {
		w.pending = &pendingPublication{delivery: delivery, body: append([]byte(nil), signed...)}
		return true, err
	}
	return true, nil
}

// Ready reports whether the result path is healthy enough to accept new work.
func (w *Worker) Ready() bool { return w.pending == nil }
func (w *Worker) JournalMetrics(ctx context.Context) (workerjournal.Metrics, error) {
	return w.journal.Metrics(ctx)
}
func (w *Worker) CleanupJournal(ctx context.Context) (int64, error) {
	return w.journal.Cleanup(ctx, w.now().UTC().Add(-7*24*time.Hour))
}
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}
func micros(v *time.Duration) *int64 {
	if v == nil {
		return nil
	}
	n := v.Microseconds()
	return &n
}
func ClockHealthy(offset, tolerance time.Duration) bool {
	if offset < 0 {
		offset = -offset
	}
	return offset <= tolerance
}
