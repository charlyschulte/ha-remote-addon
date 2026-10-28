package main

import (
	"net"
	"testing"
)

func TestIsAllowedTargetIP(t *testing.T) {
	allowed := []string{"192.168.178.10", "10.0.0.5", "172.30.33.2", "100.101.102.103", "fd12:3456::1"}
	for _, value := range allowed {
		if !isAllowedTargetIP(net.ParseIP(value)) {
			t.Errorf("%s should be allowed", value)
		}
	}
	denied := []string{"127.0.0.1", "169.254.169.254", "8.8.8.8", "0.0.0.0", "::1", "fe80::1", "1.1.1.1"}
	for _, value := range denied {
		if isAllowedTargetIP(net.ParseIP(value)) {
			t.Errorf("%s must be rejected", value)
		}
	}
}

func TestParseTargetRejectsUnsupportedURLs(t *testing.T) {
	previous := allowCustomTargets
	t.Cleanup(func() { allowCustomTargets = previous })
	allowCustomTargets = true

	if _, err := parseTarget("http://192.168.178.10:8080"); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	for _, value := range []string{"ftp://192.168.1.2", "http://user:pw@192.168.1.2", "192.168.1.2:80", "http://"} {
		if _, err := parseTarget(value); err == nil {
			t.Errorf("%q should be rejected", value)
		}
	}

	allowCustomTargets = false
	if _, err := parseTarget("http://192.168.178.10"); err == nil {
		t.Fatal("targets must be refused when disabled")
	}
}

func TestGuardedDialerRefusesPublicAddresses(t *testing.T) {
	conn, err := guardedDialer.Dial("tcp", "1.1.1.1:80")
	if err == nil {
		conn.Close()
		t.Fatal("dial to a public address must fail")
	}
}
