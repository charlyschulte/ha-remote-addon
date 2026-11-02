package main

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// haLocator keeps track of the Home Assistant port when it was detected
// automatically. If the add-on starts before Home Assistant, run.sh may pick
// the wrong port; the locator re-checks the candidate ports when Home
// Assistant refuses connections and switches to the one that answers.
type haLocator struct {
	mu         sync.Mutex
	current    *url.URL
	candidates []string
	lastProbe  time.Time
	probing    bool
}

const haReprobeInterval = 30 * time.Second

var homeAssistant = &haLocator{}

// configure sets the starting URL; candidates is a comma separated port list
// (empty when the port was configured manually).
func (l *haLocator) configure(base *url.URL, candidates string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.current = base
	l.candidates = nil
	for _, port := range strings.Split(candidates, ",") {
		if port = strings.TrimSpace(port); port != "" {
			l.candidates = append(l.candidates, port)
		}
	}
}

func (l *haLocator) base() *url.URL {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

// noticeFailure is called after a request to Home Assistant failed. When the
// connection was refused and other ports are possible, it probes them in
// the background (at most every 30 seconds).
func (l *haLocator) noticeFailure(err error) {
	if !isConnectionRefused(err) {
		return
	}
	l.mu.Lock()
	if len(l.candidates) < 2 || l.current == nil || l.probing || time.Since(l.lastProbe) < haReprobeInterval {
		l.mu.Unlock()
		return
	}
	l.probing = true
	l.lastProbe = time.Now()
	current := *l.current
	candidates := append([]string(nil), l.candidates...)
	l.mu.Unlock()

	go func() {
		found := probeHomeAssistant(&current, candidates)
		l.mu.Lock()
		defer l.mu.Unlock()
		l.probing = false
		if found != nil && found.Host != l.current.Host {
			warnf("Home Assistant does not answer on %s but on %s; switching", l.current.Host, found.Host)
			l.current = found
		}
	}()
}

// probeHomeAssistant returns the first candidate port that answers HTTP.
func probeHomeAssistant(base *url.URL, ports []string) *url.URL {
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	host := base.Hostname()
	for _, port := range ports {
		candidate := *base
		candidate.Host = net.JoinHostPort(host, port)
		probe := candidate
		probe.Path = "/manifest.json"
		resp, err := client.Get(probe.String())
		if err != nil {
			continue
		}
		resp.Body.Close()
		return &candidate
	}
	return nil
}

// isConnectionRefused also matches the Windows wording so the behaviour is
// testable everywhere; in the add-on (Linux) ECONNREFUSED matches.
func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") || strings.Contains(message, "actively refused")
}
