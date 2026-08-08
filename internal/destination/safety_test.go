package destination

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestValidateURLAppliesPhaseOnePolicy(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr error
	}{
		{name: "HTTP default port", target: "http://api.example.com/health"},
		{name: "HTTPS default port", target: "https://api.example.com/health?ready=true"},
		{name: "HTTP explicit port 443", target: "http://api.example.com:443/health"},
		{name: "HTTPS explicit port 80", target: "https://api.example.com:80/health"},
		{name: "unsupported scheme", target: "file://api.example.com/health", wantErr: ErrInvalidTarget},
		{name: "unsupported port", target: "https://api.example.com:8443/health", wantErr: ErrUnsafeTarget},
		{name: "missing port", target: "https://api.example.com:/health", wantErr: ErrInvalidTarget},
		{name: "credentials", target: "https://user:secret@api.example.com/health", wantErr: ErrInvalidTarget},
		{name: "fragment", target: "https://api.example.com/health#secret", wantErr: ErrInvalidTarget},
		{name: "localhost", target: "http://localhost/health", wantErr: ErrUnsafeTarget},
		{name: "localhost subdomain", target: "http://service.localhost/health", wantErr: ErrUnsafeTarget},
		{name: "IPv4 loopback", target: "http://127.0.0.1/health", wantErr: ErrUnsafeTarget},
		{name: "IPv6 loopback", target: "https://[::1]/health", wantErr: ErrUnsafeTarget},
		{name: "IPv6 zone", target: "http://[fe80::1%25eth0]/health", wantErr: ErrInvalidTarget},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateURL(test.target)
			if test.wantErr == nil && err != nil {
				t.Fatalf("ValidateURL() error = %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("ValidateURL() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestPublicAddressPolicyBlocksUnsafeIPv4AndIPv6(t *testing.T) {
	tests := []struct {
		address string
		public  bool
	}{
		{address: "8.8.8.8", public: true},
		{address: "2001:4860:4860::8888", public: true},
		{address: "0.0.0.0"},
		{address: "10.0.0.1"},
		{address: "100.100.100.200"}, // cloud metadata on Alibaba Cloud
		{address: "127.0.0.1"},
		{address: "169.254.169.254"}, // common cloud metadata endpoint
		{address: "172.16.0.1"},
		{address: "192.168.0.1"},
		{address: "198.18.0.1"},
		{address: "224.0.0.1"},
		{address: "255.255.255.255"},
		{address: "::"},
		{address: "::1"},
		{address: "::ffff:127.0.0.1"},
		{address: "64:ff9b::127.0.0.1"},
		{address: "fc00::1"},
		{address: "fd00:ec2::254"}, // AWS IPv6 metadata endpoint
		{address: "fe80::1"},
		{address: "ff02::1"},
	}

	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			address := netip.MustParseAddr(test.address)
			if got := isPublicAddress(address); got != test.public {
				t.Fatalf("isPublicAddress(%s) = %t, want %t", address, got, test.public)
			}
		})
	}
}

func TestHTTPClientUsesControlledDNSAndValidatedAddress(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		targetCalls.Add(1)
		if request.Host != "safe.test" {
			t.Errorf("Host = %q, want safe.test", request.Host)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	resolver := &controlledResolver{answers: map[string][]netip.Addr{
		"safe.test": {netip.MustParseAddr("8.8.8.8")},
	}}
	dialer := &targetMappingDialer{target: strings.TrimPrefix(target.URL, "http://")}
	client := NewHTTPClient(resolver, dialer)

	response, err := client.Get("http://safe.test/health")
	if err != nil {
		t.Fatalf("safe request: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("discard response: %v", err)
	}
	if response.StatusCode != http.StatusNoContent || targetCalls.Load() != 1 {
		t.Fatalf("response status = %d, target calls = %d", response.StatusCode, targetCalls.Load())
	}
	if resolver.callsFor("safe.test") != 1 {
		t.Fatalf("DNS lookups = %d, want 1", resolver.callsFor("safe.test"))
	}
	if got := dialer.lastEndpoint(); got != "8.8.8.8:80" {
		t.Fatalf("dialed endpoint = %q, want validated resolved IP", got)
	}
}

func TestHTTPClientRejectsEveryUnsafeDNSAnswerBeforeDial(t *testing.T) {
	tests := []struct {
		name    string
		answers []netip.Addr
	}{
		{name: "private IPv4", answers: []netip.Addr{netip.MustParseAddr("10.0.0.10")}},
		{name: "loopback IPv6", answers: []netip.Addr{netip.MustParseAddr("::1")}},
		{name: "metadata IPv4", answers: []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
		{name: "mixed public and private", answers: []netip.Addr{
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("192.168.1.10"),
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &controlledResolver{answers: map[string][]netip.Addr{"unsafe.test": test.answers}}
			dialer := &targetMappingDialer{target: "127.0.0.1:1"}
			client := NewHTTPClient(resolver, dialer)

			response, err := client.Get("http://unsafe.test/health")
			if response != nil {
				response.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), ErrUnsafeTarget.Error()) {
				t.Fatalf("request error = %v, want unsafe target", err)
			}
			if dialer.callCount() != 0 {
				t.Fatalf("dial calls = %d, want 0", dialer.callCount())
			}
		})
	}
}

func TestHTTPClientRejectsDisallowedPortBeforeDNS(t *testing.T) {
	resolver := &controlledResolver{answers: map[string][]netip.Addr{
		"safe.test": {netip.MustParseAddr("8.8.8.8")},
	}}
	dialer := &targetMappingDialer{target: "127.0.0.1:1"}
	client := NewHTTPClient(resolver, dialer)

	response, err := client.Get("http://safe.test:8080/health")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("request unexpectedly succeeded")
	}
	if resolver.callsFor("safe.test") != 0 || dialer.callCount() != 0 {
		t.Fatalf("unsafe port reached DNS or dial: DNS=%d dial=%d", resolver.callsFor("safe.test"), dialer.callCount())
	}
}

func TestHTTPClientDoesNotFollowRedirectsBeforeRedirectPolicyExists(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", "http://unsafe.test/latest/meta-data")
		response.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(target.Close)

	resolver := &controlledResolver{answers: map[string][]netip.Addr{
		"safe.test":   {netip.MustParseAddr("8.8.8.8")},
		"unsafe.test": {netip.MustParseAddr("169.254.169.254")},
	}}
	dialer := &targetMappingDialer{target: strings.TrimPrefix(target.URL, "http://")}
	client := NewHTTPClient(resolver, dialer)

	response, err := client.Get("http://safe.test/redirect")
	if err != nil {
		t.Fatalf("redirect response: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusFound)
	}
	if resolver.callsFor("unsafe.test") != 0 || dialer.callCount() != 1 {
		t.Fatalf(
			"redirect caused network activity: unsafe DNS=%d dial=%d",
			resolver.callsFor("unsafe.test"),
			dialer.callCount(),
		)
	}
}

type controlledResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	calls   map[string]int
}

func (resolver *controlledResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.calls == nil {
		resolver.calls = make(map[string]int)
	}
	resolver.calls[host]++
	if network != "ip" {
		return nil, errors.New("unexpected DNS network")
	}
	answers, ok := resolver.answers[host]
	if !ok {
		return nil, errors.New("controlled DNS name not found")
	}
	return append([]netip.Addr(nil), answers...), nil
}

func (resolver *controlledResolver) callsFor(host string) int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return resolver.calls[host]
}

type targetMappingDialer struct {
	target    string
	mu        sync.Mutex
	endpoints []string
}

func (dialer *targetMappingDialer) DialContext(ctx context.Context, network, endpoint string) (net.Conn, error) {
	dialer.mu.Lock()
	dialer.endpoints = append(dialer.endpoints, endpoint)
	dialer.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, dialer.target)
}

func (dialer *targetMappingDialer) callCount() int {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return len(dialer.endpoints)
}

func (dialer *targetMappingDialer) lastEndpoint() string {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	if len(dialer.endpoints) == 0 {
		return ""
	}
	return dialer.endpoints[len(dialer.endpoints)-1]
}
