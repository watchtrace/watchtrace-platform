// Package queuegateway provides a stateless, database-free HTTPS adapter over SQS.
package queuegateway

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/envelope"
	"github.com/watchtrace/watchtrace-platform/internal/workqueue"
)

const maxRequestBytes = 128 * 1024

type Pool struct {
	ID, ResultKeyID                         string
	Transport                               workqueue.Transport
	ResultPublic                            ed25519.PublicKey
	SchemaMin, SchemaMax                    int
	MaxRequestsPerMinute, MaxBytesPerMinute int64
	MaxConcurrentPulls, MaxResultsPerMinute int64
	RevokedCertificateSerials               map[string]struct{}
}
type TokenValidator func(context.Context, string) (string, bool)
type Gateway struct {
	pools                      map[string]Pool
	aead                       cipher.AEAD
	now                        func() time.Time
	tolerance                  time.Duration
	tokens                     TokenValidator
	limits                     map[string]*poolLimit
	dependencyUnavailableUntil time.Time
	mu                         sync.Mutex
}
type poolLimit struct {
	window              time.Time
	requests, bytes     int64
	results, activePull int64
}
type lease struct {
	PoolID, Receipt, JobID, SnapshotHash string
	SchemaVersion                        int
	ExpiresAt, LeaseExpiresAt            time.Time
}
type pullResponse struct {
	Body         string              `json:"body"`
	Attributes   envelope.Attributes `json:"attributes"`
	LeaseToken   string              `json:"lease_token"`
	ReceiveCount int                 `json:"receive_count"`
}
type actionRequest struct {
	LeaseToken string `json:"lease_token"`
	Body       string `json:"body,omitempty"`
	Seconds    int    `json:"seconds,omitempty"`
}

func New(pools []Pool, key []byte, tolerance time.Duration) (*Gateway, error) {
	if len(key) != 32 {
		return nil, errors.New("gateway lease key must be 32 bytes")
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	mapped := map[string]Pool{}
	limits := map[string]*poolLimit{}
	for _, p := range pools {
		if p.SchemaMin == 0 {
			p.SchemaMin = envelope.PreviousSchemaVersion
		}
		if p.SchemaMax == 0 {
			p.SchemaMax = envelope.SchemaVersion
		}
		if p.MaxRequestsPerMinute == 0 {
			p.MaxRequestsPerMinute = 120
		}
		if p.MaxBytesPerMinute == 0 {
			p.MaxBytesPerMinute = 8 * 1024 * 1024
		}
		if p.MaxConcurrentPulls == 0 {
			p.MaxConcurrentPulls = 2
		}
		if p.MaxResultsPerMinute == 0 {
			p.MaxResultsPerMinute = 120
		}
		if p.ID == "" || p.ResultKeyID == "" || p.Transport == nil || len(p.ResultPublic) != ed25519.PublicKeySize || p.SchemaMin < envelope.PreviousSchemaVersion || p.SchemaMax > envelope.SchemaVersion || p.SchemaMin > p.SchemaMax || p.MaxRequestsPerMinute < 1 || p.MaxBytesPerMinute < 1024 || p.MaxConcurrentPulls < 1 || p.MaxResultsPerMinute < 1 {
			return nil, errors.New("invalid gateway pool")
		}
		mapped[p.ID] = p
		limits[p.ID] = &poolLimit{}
	}
	return &Gateway{
		pools:     mapped,
		aead:      aead,
		now:       time.Now,
		tolerance: tolerance,
		limits:    limits,
	}, nil
}
func (g *Gateway) WithTokenValidator(v TokenValidator) *Gateway { g.tokens = v; return g }
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs/pull", g.pull)
	mux.HandleFunc("POST /v1/jobs/extend", g.extend)
	mux.HandleFunc("POST /v1/jobs/result", g.result)
	mux.HandleFunc("POST /v1/jobs/expired", g.expired)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /health/ready", g.ready)
	mux.HandleFunc("GET /metrics", g.metrics)
	return http.MaxBytesHandler(mux, maxRequestBytes)
}

func (g *Gateway) metrics(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	active := int64(0)
	for _, limit := range g.limits {
		active += limit.activePull
	}
	healthy := !g.now().UTC().Before(g.dependencyUnavailableUntil)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"configured_pools": int64(len(g.pools)), "active_pulls": active, "queue_dependency_healthy": healthy})
}

func (g *Gateway) ready(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	healthy := !g.now().UTC().Before(g.dependencyUnavailableUntil)
	g.mu.Unlock()
	if !healthy {
		http.Error(w, "queue_unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) recordDependency(err error) {
	g.mu.Lock()
	if err == nil || errors.Is(err, workqueue.ErrNoMessage) {
		g.dependencyUnavailableUntil = time.Time{}
	} else {
		g.dependencyUnavailableUntil = g.now().UTC().Add(5 * time.Second)
	}
	g.mu.Unlock()
}
func (g *Gateway) pool(r *http.Request) (Pool, bool) {
	id := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		certificate := r.TLS.PeerCertificates[0]
		id = certificate.Subject.CommonName
		if certificate.NotAfter.Sub(certificate.NotBefore) > 31*24*time.Hour || g.now().UTC().Before(certificate.NotBefore) || g.now().UTC().After(certificate.NotAfter) {
			return Pool{}, false
		}
		if configured, exists := g.pools[id]; exists {
			if _, revoked := configured.RevokedCertificateSerials[certificate.SerialNumber.String()]; revoked {
				return Pool{}, false
			}
		}
	}
	if id == "" && g.tokens != nil {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, _ = g.tokens(r.Context(), token)
	}
	p, ok := g.pools[id]
	return p, ok
}
func (g *Gateway) pull(w http.ResponseWriter, r *http.Request) {
	p, ok := g.pool(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	if !g.acquire(p, 0, true, false) {
		tooMany(w)
		return
	}
	defer g.releasePull(p.ID)
	msg, err := p.Transport.Pull(r.Context(), 20*time.Second)
	g.recordDependency(err)
	if errors.Is(err, workqueue.ErrNoMessage) {
		w.WriteHeader(204)
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	token, err := g.seal(lease{
		PoolID:         p.ID,
		Receipt:        msg.LeaseToken,
		JobID:          msg.Attributes.JobID,
		SnapshotHash:   msg.Attributes.SnapshotHash,
		SchemaVersion:  msg.Attributes.SchemaVersion,
		ExpiresAt:      msg.Attributes.ExpiresAt,
		LeaseExpiresAt: g.now().UTC().Add(2 * time.Minute),
	})
	if err != nil {
		http.Error(w, "internal", 500)
		return
	}
	writeJSON(w, pullResponse{base64.StdEncoding.EncodeToString(msg.Body), msg.Attributes, token, msg.ReceiveCount})
}
func (g *Gateway) extend(w http.ResponseWriter, r *http.Request) {
	p, msg, request, ok := g.action(w, r)
	if !ok {
		return
	}
	if request.Seconds < 1 || request.Seconds > 90 {
		http.Error(w, "invalid duration", 422)
		return
	}
	if err := p.Transport.Extend(r.Context(), msg, time.Duration(request.Seconds)*time.Second); err != nil {
		g.recordDependency(err)
		unavailable(w)
		return
	}
	g.recordDependency(nil)
	w.WriteHeader(204)
}
func (g *Gateway) result(w http.ResponseWriter, r *http.Request) {
	p, msg, request, ok := g.action(w, r)
	if !ok {
		return
	}
	body, err := base64.StdEncoding.DecodeString(request.Body)
	if err != nil || len(body) > envelope.MaxMessageBytes {
		http.Error(w, "invalid result", 422)
		return
	}
	if !g.acquire(p, int64(len(body)), false, true) {
		tooMany(w)
		return
	}
	result, err := envelope.VerifyResult(body, p.ResultPublic)
	now := g.now().UTC()
	if err != nil || result.SchemaVersion != msg.Attributes.SchemaVersion || result.SchemaVersion < p.SchemaMin || result.SchemaVersion > p.SchemaMax || result.JobID != msg.Attributes.JobID || result.SnapshotHash != msg.Attributes.SnapshotHash || result.WorkerPoolID != p.ID || result.ResultKeyID != p.ResultKeyID || result.StartedAt.Before(result.ScheduledAt.Add(-g.tolerance)) || result.StartedAt.After(msg.Attributes.ExpiresAt.Add(g.tolerance)) || result.CompletedAt.After(now.Add(g.tolerance)) {
		http.Error(w, "invalid result", 422)
		return
	}
	if err = p.Transport.PublishResultAndAcknowledge(r.Context(), msg, body); err != nil {
		g.recordDependency(err)
		unavailable(w)
		return
	}
	g.recordDependency(nil)
	w.WriteHeader(204)
}
func (g *Gateway) expired(w http.ResponseWriter, r *http.Request) {
	p, msg, request, ok := g.action(w, r)
	if !ok {
		return
	}
	body, err := base64.StdEncoding.DecodeString(request.Body)
	if err != nil {
		return
	}
	ack, err := envelope.VerifyExpired(body, p.ResultPublic)
	now := g.now().UTC()
	if err != nil || ack.SchemaVersion != msg.Attributes.SchemaVersion || ack.SchemaVersion < p.SchemaMin || ack.SchemaVersion > p.SchemaMax || ack.JobID != msg.Attributes.JobID || ack.SnapshotHash != msg.Attributes.SnapshotHash || ack.WorkerPoolID != p.ID || ack.ResultKeyID != p.ResultKeyID || now.Before(msg.Attributes.ExpiresAt.Add(-g.tolerance)) || ack.ExpiredAt.Before(msg.Attributes.ExpiresAt.Add(-g.tolerance)) || ack.ExpiredAt.After(now.Add(g.tolerance)) {
		http.Error(w, "invalid expiry", 422)
		return
	}
	if err = p.Transport.AcknowledgeExpired(r.Context(), msg, body); err != nil {
		g.recordDependency(err)
		unavailable(w)
		return
	}
	g.recordDependency(nil)
	w.WriteHeader(204)
}
func (g *Gateway) action(w http.ResponseWriter, r *http.Request) (Pool, workqueue.Delivery, actionRequest, bool) {
	p, ok := g.pool(r)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return Pool{}, workqueue.Delivery{}, actionRequest{}, false
	}
	requestBytes := r.ContentLength
	if requestBytes < 0 {
		requestBytes = 0
	}
	if !g.acquire(p, requestBytes, false, false) {
		tooMany(w)
		return Pool{}, workqueue.Delivery{}, actionRequest{}, false
	}
	var request actionRequest
	if decode(r.Body, &request) != nil {
		http.Error(w, "invalid request", 400)
		return Pool{}, workqueue.Delivery{}, request, false
	}
	l, err := g.open(request.LeaseToken)
	if err != nil || l.PoolID != p.ID {
		http.Error(w, "invalid lease", 401)
		return Pool{}, workqueue.Delivery{}, request, false
	}
	msg := workqueue.Delivery{LeaseToken: l.Receipt, Attributes: envelope.Attributes{SchemaVersion: l.SchemaVersion, JobID: l.JobID, WorkerPoolID: l.PoolID, SnapshotHash: l.SnapshotHash, ExpiresAt: l.ExpiresAt}}
	return p, msg, request, true
}

func (g *Gateway) acquire(pool Pool, bytes int64, pull, result bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	limit := g.limits[pool.ID]
	now := g.now().UTC().Truncate(time.Minute)
	if limit.window.IsZero() || !limit.window.Equal(now) {
		limit.window, limit.requests, limit.bytes, limit.results = now, 0, 0, 0
	}
	if limit.requests+1 > pool.MaxRequestsPerMinute || limit.bytes+bytes > pool.MaxBytesPerMinute || (pull && limit.activePull >= pool.MaxConcurrentPulls) || (result && limit.results+1 > pool.MaxResultsPerMinute) {
		return false
	}
	limit.requests++
	limit.bytes += bytes
	if pull {
		limit.activePull++
	}
	if result {
		limit.results++
	}
	return true
}

func (g *Gateway) releasePull(poolID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit := g.limits[poolID]; limit != nil && limit.activePull > 0 {
		limit.activePull--
	}
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(1))
	http.Error(w, "pool limit exceeded", http.StatusTooManyRequests)
}
func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
}
func (g *Gateway) seal(value lease) (string, error) {
	plain, _ := json.Marshal(value)
	nonce := make([]byte, g.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(g.aead.Seal(nonce, nonce, plain, nil)), nil
}
func (g *Gateway) open(token string) (lease, error) {
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) < g.aead.NonceSize() {
		return lease{}, errors.New("invalid lease")
	}
	plain, err := g.aead.Open(nil, data[:g.aead.NonceSize()], data[g.aead.NonceSize():], nil)
	if err != nil {
		return lease{}, errors.New("invalid lease")
	}
	var value lease
	if json.Unmarshal(plain, &value) != nil || value.Receipt == "" || value.JobID == "" || value.LeaseExpiresAt.IsZero() || g.now().UTC().After(value.LeaseExpiresAt.Add(g.tolerance)) {
		return lease{}, errors.New("invalid lease")
	}
	return value, nil
}
func decode(r io.Reader, v any) error {
	decoder := json.NewDecoder(io.LimitReader(r, maxRequestBytes))
	decoder.DisallowUnknownFields()
	return decoder.Decode(v)
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var _ = context.Background
