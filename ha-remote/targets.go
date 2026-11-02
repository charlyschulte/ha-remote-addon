package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Custom targets let a subdomain forward to another service in the user's
// network (e.g. http://192.168.178.10:8080) instead of Home Assistant. The
// target comes from the HA Remote dashboard, so the agent only ever connects
// to private network addresses: a compromised server must not be able to use
// the agent to reach the internet or the add-on itself.

var allowCustomTargets = true

var carrierGradeNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isAllowedTargetIP accepts private LAN ranges (RFC 1918, IPv6 ULA) and the
// CGNAT range used by VPNs such as Tailscale. Loopback, link-local (cloud
// metadata) and public addresses are rejected.
func isAllowedTargetIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return ip.IsPrivate() || carrierGradeNAT.Contains(ip)
}

// parseTarget validates a target base URL from the server.
func parseTarget(raw string) (*url.URL, error) {
	if !allowCustomTargets {
		return nil, fmt.Errorf("custom targets are disabled in the add-on configuration")
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid target: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("target must use http or https")
	}
	if parsed.Hostname() == "" || parsed.User != nil {
		return nil, fmt.Errorf("invalid target host")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

// guardedDialer only completes connections to allowed addresses. The check
// runs after DNS resolution, so hostnames cannot be used to sneak around it.
var guardedDialer = &net.Dialer{
	Timeout:   10 * time.Second,
	KeepAlive: 30 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !isAllowedTargetIP(net.ParseIP(host)) {
			return fmt.Errorf("target %s is not a private network address", host)
		}
		return nil
	},
}

// Services in a home network almost always use self-signed certificates;
// the traffic never leaves the LAN, so certificates are not verified.
var targetTLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec

func newTargetClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           guardedDialer.DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       targetTLSConfig,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func targetWebSocketDialer() *websocket.Dialer {
	return &websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return guardedDialer.DialContext(ctx, network, address)
		},
		TLSClientConfig: targetTLSConfig,
	}
}

// upstreamFor picks the base URL, HTTP client and Host header for a request:
// Home Assistant by default, or the custom target from the route.
func (a *agentRuntime) upstreamFor(target string) (*url.URL, *http.Client, bool, error) {
	if target == "" {
		if base := homeAssistant.base(); base != nil {
			return base, a.httpConn, false, nil
		}
		return a.haBaseURL, a.httpConn, false, nil
	}
	parsed, err := parseTarget(target)
	if err != nil {
		return nil, nil, true, err
	}
	a.mu.Lock()
	if a.targetConn == nil {
		a.targetConn = newTargetClient()
	}
	client := a.targetConn
	a.mu.Unlock()
	return parsed, client, true, nil
}
