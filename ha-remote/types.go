package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type pairResponse struct {
	TunnelToken     string `json:"tunnelToken"`
	ConnectURL      string `json:"connectUrl"`
	PairingCodeHash string `json:"pairingCodeHash,omitempty"`
}

type tunnelMessage struct {
	Type   string `json:"type"`
	ReqID  string `json:"reqId,omitempty"`
	ConnID string `json:"connId,omitempty"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	// RawPath is the escaped request path; Path stays decoded for older agents.
	RawPath string              `json:"rawPath,omitempty"`
	Query   string              `json:"query,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
	Status  int                 `json:"status,omitempty"`
	Binary  bool                `json:"binary,omitempty"`
	Stream  bool                `json:"stream,omitempty"`
	Error   string              `json:"error,omitempty"`
	// Target is a custom service URL in the user's network; empty means
	// Home Assistant.
	Target string `json:"target,omitempty"`
	// Flow is set by proxies that acknowledge streamed chunks with
	// http_ack messages; the agent then limits unacknowledged bytes.
	Flow      bool     `json:"flow,omitempty"`
	Ack       int64    `json:"ack,omitempty"`
	Protocols []string `json:"protocols,omitempty"`
}

type agentRuntime struct {
	conn     *websocket.Conn
	send     chan tunnelMessage
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	haBaseURL *url.URL
	httpConn  *http.Client
	// targetConn talks to custom targets (private addresses only).
	targetConn *http.Client

	mu         sync.Mutex
	upstreamWS map[string]*upstreamWebSocket
	httpCancel map[string]context.CancelFunc
	httpAcks   map[string]chan int64
}

// upstreamWebSocket is one browser websocket bridged to Home Assistant.
// Frames from the browser are queued and written by a dedicated goroutine,
// so dialing or a slow Home Assistant never blocks the tunnel read loop.
type upstreamWebSocket struct {
	connID    string
	out       chan tunnelMessage
	done      chan struct{}
	closeOnce sync.Once

	mu   sync.Mutex
	conn *websocket.Conn
}

func (u *upstreamWebSocket) close() {
	u.closeOnce.Do(func() {
		close(u.done)
		u.mu.Lock()
		conn := u.conn
		u.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
}

const (
	// Keep the tunnel alive through NAT/load balancer idle timeouts and
	// notice dead connections instead of waiting forever.
	pingInterval = 25 * time.Second
	pongWait     = 70 * time.Second
	writeWait    = 15 * time.Second

	// flowWindow bounds the streamed bytes that may be in flight towards a
	// browser before the proxy acknowledges them.
	flowWindow = 1 << 20

	upstreamWSQueue    = 512
	nonStreamTimeout   = 5 * time.Minute
	maxReconnectDelay  = 30 * time.Second
	tokenRejectedDelay = 60 * time.Second
)

var errTokenRejected = errors.New("tunnel token rejected by the server")

const (
	originalHostHeader  = "X-Haremote-Original-Host"
	originalProtoHeader = "X-Haremote-Original-Proto"
)

var strippedHeaders = map[string]struct{}{
	"x-forwarded-for":   {},
	"x-forwarded-host":  {},
	"x-forwarded-proto": {},
	"x-real-ip":         {},
	"forwarded":         {},
	"cf-connecting-ip":  {},
	"true-client-ip":    {},
}
