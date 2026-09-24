package ingest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// newSafeHTTPClient returns an HTTP client that refuses private/metadata
// targets (SSRF defense). The check runs at dial time against the IPs actually
// connected to, so it covers every redirect hop and DNS rebinding alike.
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Never honor HTTP(S)_PROXY: a proxy hop would bypass the dial-time IP checks.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			last := fmt.Errorf("no usable addresses for %s", host)
			for _, ip := range ips {
				if last = rejectIP(ip); last != nil {
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, last
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return validateFetchURL(req.URL.String())
		},
	}
}

// validateFetchURL permits only http/https URLs with a real hostname and no userinfo.
func validateFetchURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("url scheme %q not allowed", u.Scheme)
	}
	if u.User != nil {
		return errors.New("url userinfo not allowed")
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "":
		return errors.New("url host required")
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return errors.New("localhost not allowed")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return rejectIP(ip) // literal IPs fail fast, before any dial
	}
	return nil
}

// blockedPrefixes are non-public ranges not covered by the netip predicates.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved + broadcast
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 (maps onto IPv4, incl. private)
}

func rejectIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("blocked address %s", ip)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return fmt.Errorf("blocked address %s", ip)
		}
	}
	return nil
}
