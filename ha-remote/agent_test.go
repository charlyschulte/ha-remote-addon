package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

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
			originalHostHeader: {"hadev.home.ctech.media"},
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
	case <-time.After(time.Second):
		t.Fatal("upstream request not received")
	}

	response := <-runtime.send
	if response.Status != http.StatusOK || response.Error != "" {
		t.Fatalf("tunnel response = %+v", response)
	}
}
