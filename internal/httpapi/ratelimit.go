package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type rateBucket struct {
	started time.Time
	count   int
}
type RateLimits struct{ AuthPerMinute, MutationsPerMinute, ManualPerMinute, ReportsPerMinute, InvitesPerHour, LiveConnections int }
type RateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	limits  RateLimits
	buckets map[string]rateBucket
	live    map[string]int
}

func NewRateLimiter(l RateLimits) *RateLimiter {
	if l.AuthPerMinute < 1 {
		l.AuthPerMinute = 30
	}
	if l.MutationsPerMinute < 1 {
		l.MutationsPerMinute = 120
	}
	if l.ManualPerMinute < 1 {
		l.ManualPerMinute = 10
	}
	if l.ReportsPerMinute < 1 {
		l.ReportsPerMinute = 120
	}
	if l.InvitesPerHour < 1 {
		l.InvitesPerHour = 20
	}
	if l.LiveConnections < 1 {
		l.LiveConnections = 3
	}
	return &RateLimiter{now: time.Now, limits: l, buckets: map[string]rateBucket{}, live: map[string]int{}}
}
func (r *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		class, limit, window, live := r.classify(c.Request.Method, c.Request.URL.Path)
		if class == "" {
			c.Next()
			return
		}
		client := clientIdentity(c.Request)
		key := class + ":" + client
		if live {
			if !r.acquireLive(key) {
				rateLimitResponse(c, 1)
				return
			}
			defer r.releaseLive(key)
			c.Next()
			return
		}
		allowed, retry := r.allow(key, limit, window)
		if !allowed {
			rateLimitResponse(c, retry)
			return
		}
		c.Next()
	}
}

// clientIdentity keeps authenticated callers behind the same reverse proxy in
// separate buckets without trusting caller-controlled forwarding headers. Raw
// bearer and refresh tokens are never retained in the limiter's in-memory
// bucket keys.
func clientIdentity(request *http.Request) string {
	if cookie, err := request.Cookie(refreshTokenCookieName); err == nil && cookie.Value != "" {
		return "refresh:" + identityDigest(cookie.Value)
	}
	// Authentication endpoints do not authenticate bearer tokens. Ignoring an
	// arbitrary Authorization header here prevents callers from selecting a new
	// rate-limit bucket for every login or password-reset attempt.
	if !strings.HasPrefix(request.URL.Path, "/api/v1/auth/") {
		if token, ok := bearerToken(request.Header.Get("Authorization")); ok {
			return "session:" + identityDigest(token)
		}
	}
	return "address:" + clientAddress(request)
}

func identityDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func (r *RateLimiter) classify(method, path string) (string, int, time.Duration, bool) {
	switch {
	case strings.HasSuffix(path, "/events"):
		return "live", r.limits.LiveConnections, 0, true
	case strings.HasPrefix(path, "/api/v1/auth/") && path != "/api/v1/auth/me" && path != "/api/v1/auth/logout":
		return "auth", r.limits.AuthPerMinute, time.Minute, false
	case strings.HasSuffix(path, "/test"):
		return "manual", r.limits.ManualPerMinute, time.Minute, false
	case strings.HasSuffix(path, "/invitations"):
		return "invite", r.limits.InvitesPerHour, time.Hour, false
	case strings.HasSuffix(path, "/report") || strings.HasSuffix(path, "/dashboard"):
		return "report", r.limits.ReportsPerMinute, time.Minute, false
	case method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete:
		return "mutation", r.limits.MutationsPerMinute, time.Minute, false
	default:
		return "", 0, 0, false
	}
}
func (r *RateLimiter) allow(key string, limit int, window time.Duration) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	bucket := r.buckets[key]
	if bucket.started.IsZero() || now.Sub(bucket.started) >= window {
		bucket = rateBucket{started: now}
	}
	if bucket.count >= limit {
		retry := int(math.Ceil((window - now.Sub(bucket.started)).Seconds()))
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	bucket.count++
	r.buckets[key] = bucket
	return true, 0
}
func (r *RateLimiter) acquireLive(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[key] >= r.limits.LiveConnections {
		return false
	}
	r.live[key]++
	return true
}
func (r *RateLimiter) releaseLive(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[key]--
	if r.live[key] <= 0 {
		delete(r.live, key)
	}
}
func clientAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	if request.RemoteAddr != "" {
		return request.RemoteAddr
	}
	return "unknown"
}
func rateLimitResponse(c *gin.Context, retry int) {
	c.Header("Retry-After", strconv.Itoa(retry))
	RespondError(c, http.StatusTooManyRequests, "rate_limited", "request rate limit exceeded")
}
