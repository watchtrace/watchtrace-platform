// Package destination enforces the network boundary for monitor targets.
package destination

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

var (
	// ErrInvalidTarget indicates that a target URL cannot be used by a Phase 1
	// monitor. The error intentionally does not include the target URL.
	ErrInvalidTarget = errors.New("invalid monitor target")
	// ErrUnsafeTarget indicates that a syntactically valid target could reach a
	// destination outside the public network boundary.
	ErrUnsafeTarget = errors.New("unsafe monitor target")
	// ErrResolutionFailed indicates that DNS did not return usable addresses.
	ErrResolutionFailed = errors.New("monitor target resolution failed")
	// ErrConnectionFailed indicates that every validated address failed to dial.
	ErrConnectionFailed = errors.New("monitor target connection failed")
)

// Resolver is the subset of net.Resolver used at the connection boundary.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// ContextDialer is the subset of net.Dialer used after address validation.
type ContextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// ValidateURL applies the Phase 1 URL policy without performing DNS. DNS is
// deliberately validated again by the guarded dialer immediately before a
// connection, because a hostname's addresses may change after configuration.
func ValidateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" ||
		parsed.User != nil || parsed.Fragment != "" {
		return ErrInvalidTarget
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return ErrInvalidTarget
	}

	host := parsed.Hostname()
	if host == "" || strings.HasSuffix(parsed.Host, ":") || strings.Contains(host, "%") {
		return ErrInvalidTarget
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return ErrUnsafeTarget
	}

	canonicalHost := strings.TrimSuffix(strings.ToLower(host), ".")
	if canonicalHost == "localhost" || strings.HasSuffix(canonicalHost, ".localhost") {
		return ErrUnsafeTarget
	}

	if address, parseErr := netip.ParseAddr(host); parseErr == nil && !isPublicAddress(address) {
		return ErrUnsafeTarget
	}
	return nil
}

// NewHTTPClient returns an HTTP client whose connections are pinned to IPs
// that were resolved and validated immediately before dialing. A nil resolver
// or dialer selects the standard library implementation. Environment proxy
// settings are not used because a proxy would move validation away from the
// actual monitor destination. Redirects remain disabled until the complete
// redirect policy is implemented in P1-303.
func NewHTTPClient(resolver Resolver, dialer ContextDialer) *http.Client {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}

	guard := guardedDialer{resolver: resolver, dialer: dialer}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guard.DialContext

	return &http.Client{
		Transport: validatingTransport{base: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type validatingTransport struct {
	base http.RoundTripper
}

func (transport validatingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil || ValidateURL(request.URL.String()) != nil {
		return nil, ErrUnsafeTarget
	}
	return transport.base.RoundTrip(request)
}

type guardedDialer struct {
	resolver Resolver
	dialer   ContextDialer
}

func (guard guardedDialer) DialContext(ctx context.Context, network, endpoint string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, ErrInvalidTarget
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || (port != "80" && port != "443") {
		return nil, ErrUnsafeTarget
	}

	addresses, err := guard.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	var attempted bool
	for _, address := range addresses {
		if network == "tcp4" && !address.Is4() {
			continue
		}
		if network == "tcp6" && address.Is4() {
			continue
		}
		attempted = true
		connection, dialErr := guard.dialer.DialContext(
			ctx,
			network,
			net.JoinHostPort(address.String(), port),
		)
		if dialErr == nil {
			return connection, nil
		}
	}
	if !attempted {
		return nil, ErrResolutionFailed
	}
	return nil, ErrConnectionFailed
}

func (guard guardedDialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		literal = literal.Unmap()
		if !isPublicAddress(literal) {
			return nil, ErrUnsafeTarget
		}
		return []netip.Addr{literal}, nil
	}

	addresses, err := guard.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrResolutionFailed
	}

	validated := make([]netip.Addr, 0, len(addresses))
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !isPublicAddress(address) {
			return nil, ErrUnsafeTarget
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		validated = append(validated, address)
	}
	if len(validated) == 0 {
		return nil, ErrResolutionFailed
	}
	return validated, nil
}

func isPublicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range deniedSpecialUsePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// These globally addressed ranges are not ordinary public destinations. They
// include carrier-grade/private translation, benchmarking, documentation,
// protocol-assignment, and IPv4-embedding ranges that could otherwise bypass
// a direct private-address check. Link-local metadata addresses and private
// IPv6 metadata addresses are already rejected by isPublicAddress.
var deniedSpecialUsePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}
