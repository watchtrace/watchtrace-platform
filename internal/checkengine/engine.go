// Package checkengine executes bounded HTTP checks without database or queue dependencies.
package checkengine

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/watchtrace/watchtrace-platform/internal/destination"
)

const UserAgent = "WatchTrace-Phase1/1.0"

type Request struct {
	JobID, URL, Method       string
	Headers                  map[string]string
	Timeout                  time.Duration
	ExpectedMin, ExpectedMax int
	MaxResponseBytes         int64
}
type Result struct {
	StartedAt, CompletedAt       time.Time
	Succeeded                    bool
	StatusCode                   *int16
	ErrorCategory                *string
	DNS, Connect, TLS, FirstByte *time.Duration
	Total                        time.Duration
}
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}
type Engine struct {
	client Doer
	now    func() time.Time
}

func New(policy destination.Policy, resolver destination.Resolver, dialer destination.ContextDialer) *Engine {
	return &Engine{client: destination.NewHTTPClientWithPolicy(resolver, dialer, policy), now: time.Now}
}
func NewWithClient(client Doer) *Engine { return &Engine{client: client, now: time.Now} }

func (e *Engine) Execute(ctx context.Context, input Request) (Result, error) {
	if input.JobID == "" || (input.Method != "GET" && input.Method != "HEAD") || input.Timeout < time.Second || input.Timeout > 10*time.Second || input.ExpectedMin < 100 || input.ExpectedMax > 599 || input.ExpectedMin > input.ExpectedMax || input.MaxResponseBytes < 1 || input.MaxResponseBytes > 1024*1024 {
		return Result{}, errors.New("invalid check request")
	}
	start := e.now()
	reqctx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqctx, input.Method, input.URL, nil)
	if err != nil {
		return finish(start, e.now(), 0, "invalid_target", nil), nil
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("X-WatchTrace-Job-ID", input.JobID)
	for k, v := range input.Headers {
		req.Header.Set(k, v)
	}
	var dnsStart, connStart, tlsStart time.Time
	var dns, conn, tlsd, first *time.Duration
	trace := &httptrace.ClientTrace{DNSStart: func(httptrace.DNSStartInfo) { dnsStart = e.now() }, DNSDone: func(httptrace.DNSDoneInfo) { dns = durationPtr(e.now().Sub(dnsStart)) }, ConnectStart: func(_, _ string) { connStart = e.now() }, ConnectDone: func(_, _ string, _ error) { conn = durationPtr(e.now().Sub(connStart)) }, TLSHandshakeStart: func() { tlsStart = e.now() }, TLSHandshakeDone: func(tls.ConnectionState, error) { tlsd = durationPtr(e.now().Sub(tlsStart)) }, GotFirstResponseByte: func() { first = durationPtr(e.now().Sub(start)) }}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	response, requestErr := e.client.Do(req)
	timings := []*time.Duration{dns, conn, tlsd, first}
	if requestErr != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("check interrupted: %w", ctx.Err())
		}
		return finish(start, e.now(), 0, categorize(requestErr), timings), nil
	}
	if response == nil {
		return finish(start, e.now(), 0, "http_protocol", timings), nil
	}
	status := response.StatusCode
	read, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, input.MaxResponseBytes+1))
	closeErr := response.Body.Close()
	if read > input.MaxResponseBytes {
		return finish(start, e.now(), status, "response_too_large", timings), nil
	}
	if readErr != nil || closeErr != nil {
		return finish(start, e.now(), status, "response_body", timings), nil
	}
	if status < input.ExpectedMin || status > input.ExpectedMax {
		return finish(start, e.now(), status, "unexpected_status", timings), nil
	}
	return finish(start, e.now(), status, "", timings), nil
}
func finish(start, end time.Time, status int, category string, t []*time.Duration) Result {
	if end.Before(start) {
		end = start
	}
	r := Result{StartedAt: start, CompletedAt: end, Succeeded: category == "", Total: end.Sub(start)}
	if status > 0 {
		s := int16(status)
		r.StatusCode = &s
	}
	if category != "" {
		r.ErrorCategory = &category
	}
	if len(t) == 4 {
		r.DNS = t[0]
		r.Connect = t[1]
		r.TLS = t[2]
		r.FirstByte = t[3]
	}
	return r
}
func durationPtr(v time.Duration) *time.Duration {
	if v < 0 {
		v = 0
	}
	return &v
}
func categorize(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, destination.ErrUnsafeTarget):
		return "unsafe_target"
	case errors.Is(err, destination.ErrResolutionFailed):
		return "dns"
	case errors.Is(err, destination.ErrConnectionFailed):
		return "connection"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	if errors.As(err, &cert) || errors.As(err, &ua) || errors.As(err, &hn) {
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "timeout"
		}
		return "connection"
	}
	if strings.Contains(strings.ToLower(err.Error()), "redirect") {
		return "redirect_limit"
	}
	return "http_protocol"
}
