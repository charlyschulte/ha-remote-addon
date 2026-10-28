package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestValidatedOriginalHost(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		want      string
	}{
		{name: "remote host", candidate: "hadev.home.ctech.media", want: "hadev.home.ctech.media"},
		{name: "host with port", candidate: "hadev.home.ctech.media:443", want: "hadev.home.ctech.media:443"},
		{name: "missing", candidate: "", want: "homeassistant:8123"},
		{name: "header injection", candidate: "example.test\r\nX-Evil: true", want: "homeassistant:8123"},
		{name: "path is invalid", candidate: "example.test/path", want: "homeassistant:8123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validatedOriginalHost(tt.candidate, "homeassistant:8123"); got != tt.want {
				t.Fatalf("validatedOriginalHost(%q) = %q, want %q", tt.candidate, got, tt.want)
			}
		})
	}
}

func TestFirstHeaderValueIsCaseInsensitive(t *testing.T) {
	headers := map[string][]string{"x-haremote-original-host": {"hadev.home.ctech.media"}}
	if got := firstHeaderValue(headers, originalHostHeader); got != "hadev.home.ctech.media" {
		t.Fatalf("firstHeaderValue() = %q", got)
	}
}

func TestHandleHTTPPreservesHostPathAndQuery(t *testing.T) {
	received := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Clone(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	baseURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &agentRuntime{
		haBaseURL: baseURL,
		httpConn:  upstream.Client(),
		send:      make(chan tunnelMessage, 1),
		done:      make(chan struct{}),
	}

	runtime.handleHTTP(tunnelMessage{
		Type:   "http_req",
		ReqID:  "request-1",
		Method: http.MethodGet,
		Path:   "/hacsfiles/ha-mammotion-assets/geojson.js",
		Query:  "hacstag=10930090459773",
		Headers: map[string][]string{
			originalHostHeader:  {"hadev.home.ctech.media"},
			originalProtoHeader: {"https"},
			"X-Forwarded-Host":  {"spoofed.example"},
			"X-Forwarded-Proto": {"javascript"},
		},
	})

	select {
	case request := <-received:
		if request.Host != "hadev.home.ctech.media" {
			t.Fatalf("upstream Host = %q", request.Host)
		}
		if request.URL.Path != "/hacsfiles/ha-mammotion-assets/geojson.js" {
			t.Fatalf("upstream path = %q", request.URL.Path)
		}
		if request.URL.RawQuery != "hacstag=10930090459773" {
			t.Fatalf("upstream query = %q", request.URL.RawQuery)
		}
		if request.Header.Get(originalHostHeader) != "" {
			t.Fatalf("private tunnel header leaked upstream")
		}
		if request.Header.Get(originalProtoHeader) != "" {
			t.Fatalf("private tunnel proto header leaked upstream")
		}
		if request.Header.Get("X-Forwarded-Host") != "hadev.home.ctech.media" {
			t.Fatalf("X-Forwarded-Host = %q", request.Header.Get("X-Forwarded-Host"))
		}
		if request.Header.Get("X-Forwarded-Proto") != "https" {
			t.Fatalf("X-Forwarded-Proto = %q", request.Header.Get("X-Forwarded-Proto"))
		}
	case <-time.After(time.Second):
		t.Fatal("upstream request not received")
	}

	response := <-runtime.send
	if response.Status != http.StatusOK || response.Error != "" {
		t.Fatalf("tunnel response = %+v", response)
	}
}

func TestValidatedOriginalProto(t *testing.T) {
	tests := []struct {
		candidate string
		want      string
	}{
		{candidate: "https", want: "https"},
		{candidate: "HTTP", want: "http"},
		{candidate: "javascript", want: "http"},
		{candidate: "", want: "http"},
	}

	for _, tt := range tests {
		if got := validatedOriginalProto(tt.candidate); got != tt.want {
			t.Fatalf("validatedOriginalProto(%q) = %q, want %q", tt.candidate, got, tt.want)
		}
	}
}

func TestDoUpstreamRequestRetriesGETAfterEOF(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, io.EOF
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString("ok")),
			Request:    request,
		}, nil
	})}
	request, err := http.NewRequest(http.MethodGet, "http://homeassistant:8123/favicon.ico", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := doUpstreamRequest(client, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDoUpstreamRequestDoesNotRetryPOSTAfterEOF(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, io.EOF
	})}
	request, err := http.NewRequest(http.MethodPost, "http://homeassistant:8123/api/services/test", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := doUpstreamRequest(client, request); err == nil {
		t.Fatal("expected EOF")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestUpstreamClientDoesNotFollowRedirects(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("target body"))
	}))
	defer upstream.Close()

	baseURL, _ := url.Parse(upstream.URL)
	runtime := &agentRuntime{
		haBaseURL: baseURL,
		httpConn:  newUpstreamClient(),
		send:      make(chan tunnelMessage, 1),
		done:      make(chan struct{}),
	}
	runtime.handleHTTP(tunnelMessage{Type: "http_req", ReqID: "r", Method: http.MethodGet, Path: "/start"})

	response := <-runtime.send
	if response.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302 passed through to the browser", response.Status)
	}
	if got := response.Headers["Location"]; len(got) != 1 || got[0] != "/target" {
		t.Fatalf("Location = %v", got)
	}
}

func TestSetUpstreamPathKeepsEncodedSlashes(t *testing.T) {
	base, _ := url.Parse("http://homeassistant:8123")
	target := *base
	setUpstreamPath(&target, base, "/api/media/a/b", "/api/media/a%2Fb")
	if got := target.String(); got != "http://homeassistant:8123/api/media/a%2Fb" {
		t.Fatalf("upstream URL = %q", got)
	}

	legacy := *base
	setUpstreamPath(&legacy, base, "/lovelace/0", "")
	if got := legacy.String(); got != "http://homeassistant:8123/lovelace/0" {
		t.Fatalf("legacy upstream URL = %q", got)
	}
}

func TestStreamingWaitsForFlowControlAcks(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 3*flowWindow)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	baseURL, _ := url.Parse(upstream.URL)
	runtime := &agentRuntime{
		haBaseURL: baseURL,
		httpConn:  newUpstreamClient(),
		send:      make(chan tunnelMessage, 1024),
		done:      make(chan struct{}),
	}
	finished := make(chan struct{})
	go func() {
		runtime.handleHTTP(tunnelMessage{Type: "http_req", ReqID: "s", Method: http.MethodGet, Path: "/big.bin", Stream: true, Flow: true})
		close(finished)
	}()

	// Without acks the agent must stop after roughly one window.
	time.Sleep(300 * time.Millisecond)
	sent := 0
	for len(runtime.send) > 0 {
		msg := <-runtime.send
		if msg.Type == "http_chunk" {
			decoded, _ := base64.RawStdEncoding.DecodeString(msg.Body)
			sent += len(decoded)
		}
	}
	if sent > flowWindow+64*1024 {
		t.Fatalf("agent sent %d bytes without acks, window is %d", sent, flowWindow)
	}

	// Acknowledging everything lets the stream finish.
	go func() {
		for {
			select {
			case msg := <-runtime.send:
				if msg.Type == "http_chunk" {
					decoded, _ := base64.RawStdEncoding.DecodeString(msg.Body)
					runtime.ackHTTP("s", int64(len(decoded)))
				}
			case <-finished:
				return
			}
		}
	}()
	runtime.ackHTTP("s", int64(sent))
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not finish after acks")
	}
}
