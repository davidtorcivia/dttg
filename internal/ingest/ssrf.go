package ingest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// newSafeHTTPClient returns an HTTP client that rejects private/metadata targets
// both at the initial URL and on every redirect hop (SSRF defense).
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// Never honor HTTP(S)_PROXY: a proxy hop would bypass our dial-time IP checks.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, ipa := range ips {
				if err := rejectIP(ipa.IP); err != nil {
					last = err
					continue
				}
				addr := net.JoinHostPort(ipa.IP.String(), port)
				conn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					last = err
					continue
				}
				return conn, nil
			}
			if last == nil {
				last = fmt.Errorf("no usable addresses for %s", host)
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
				return fmt.Errorf("stopped after 5 redirects")
			}
			if _, err := validateFetchURL(req.URL.String()); err != nil {
				return err
			}
			// Re-resolve and reject private IPs for the redirect target host.
			host := req.URL.Hostname()
			ips, err := net.DefaultResolver.LookupIPAddr(req.Context(), host)
			if err != nil {
				return err
			}
			for _, ipa := range ips {
				if err := rejectIP(ipa.IP); err != nil {
					return fmt.Errorf("redirect to blocked address: %w", err)
				}
			}
			return nil
		},
	}
}

// validateFetchURL permits only http/https URLs with a real hostname and no userinfo.
func validateFetchURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("url scheme %q not allowed", u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("url userinfo not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url host required")
	}
	lh := strings.ToLower(host)
	if lh == "localhost" || strings.HasSuffix(lh, ".localhost") {
		return nil, fmt.Errorf("localhost not allowed")
	}
	// Literal IP hostnames are checked here too (before dial).
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := rejectIP(net.IP(ip.AsSlice())); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func rejectIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("empty ip")
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return fmt.Errorf("invalid ip")
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return fmt.Errorf("blocked address %s", addr)
	}
	// Unique-local IPv6 (fc00::/7) — covered by IsPrivate in recent Go, but keep explicit.
	if addr.Is6() {
		b := addr.As16()
		if b[0]&0xfe == 0xfc {
			return fmt.Errorf("blocked address %s", addr)
		}
	}
	// CGNAT 100.64.0.0/10
	if ip4 := addr.AsSlice(); len(ip4) == 4 {
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return fmt.Errorf("blocked address %s", addr)
		}
		// Benchmarking 198.18.0.0/15
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return fmt.Errorf("blocked address %s", addr)
		}
		// Metadata 169.254.169.254 (link-local also catches 169.254/16, but be explicit)
		if ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 169 && ip4[3] == 254 {
			return fmt.Errorf("blocked address %s", addr)
		}
	}
	return nil
}
