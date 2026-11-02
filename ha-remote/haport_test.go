package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestLocatorSwitchesToAnsweringPort(t *testing.T) {
	ha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer ha.Close()
	_, goodPort, _ := net.SplitHostPort(ha.Listener.Addr().String())

	// A port nobody listens on, standing in for the wrongly detected one.
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, badPort, _ := net.SplitHostPort(closed.Addr().String())
	closed.Close()

	start, _ := url.Parse("http://127.0.0.1:" + badPort)
	locator := &haLocator{}
	locator.configure(start, badPort+","+goodPort)

	_, err := http.Get(start.String())
	if err == nil {
		t.Fatal("expected connection refused on the closed port")
	}
	locator.noticeFailure(err)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if locator.base().Port() == goodPort {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("locator still on %s, want port %s", locator.base().Host, goodPort)
}

func TestLocatorIgnoresManualPort(t *testing.T) {
	start, _ := url.Parse("http://127.0.0.1:1")
	locator := &haLocator{}
	locator.configure(start, "")
	locator.noticeFailure(fmt.Errorf("wrapped: %w", errConnRefusedForTest()))
	time.Sleep(50 * time.Millisecond)
	if locator.base().Host != "127.0.0.1:1" || locator.probing {
		t.Fatal("manual port must never be changed")
	}
}

func errConnRefusedForTest() error {
	_, err := net.DialTimeout("tcp", "127.0.0.1:1", time.Second)
	return err
}
