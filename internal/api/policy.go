package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"lakesync/internal/config"
)

// egressIP looks up the server's public IP once (for the Atlas allowlist hint).
type egressIP struct {
	mu sync.RWMutex
	ip string
}

func (e *egressIP) get() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ip
}

func (e *egressIP) lookup(ctx context.Context, cfg *config.Config) {
	if cfg.PublicIP != "" {
		e.mu.Lock()
		e.ip = cfg.PublicIP
		e.mu.Unlock()
		return
	}
	if cfg.DisableIPLookup {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(lctx, "GET", "https://api.ipify.org", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil {
		e.mu.Lock()
		e.ip = ip.String()
		e.mu.Unlock()
	}
}

// checkEndpoint enforces the outbound policy: never link-local or cloud metadata
// addresses; private ranges only when ALLOW_PRIVATE_ENDPOINTS is on.
func (s *Server) checkEndpoint(ctx context.Context, raw string) error {
	host := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid URL %q", raw)
		}
		host = u.Hostname()
	} else if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	if host == "" {
		return fmt.Errorf("invalid endpoint %q", raw)
	}
	lower := strings.ToLower(host)
	if lower == "metadata.google.internal" || lower == "metadata" || strings.HasSuffix(lower, ".internal") && strings.Contains(lower, "metadata") {
		return fmt.Errorf("%s is a cloud metadata address and is not allowed", host)
	}
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(rctx, host)
		if err != nil {
			return nil // unresolvable now; the connection test will report it properly
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
			return fmt.Errorf("%s resolves to a link-local or cloud metadata address (%s), which is not allowed", host, ip)
		}
		if !s.cfg.AllowPrivateEndpoints && (ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified()) {
			return fmt.Errorf("%s resolves to a private address (%s); set ALLOW_PRIVATE_ENDPOINTS=true to allow private networks", host, ip)
		}
	}
	return nil
}
