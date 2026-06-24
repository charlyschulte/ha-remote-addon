package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type pairResponse struct {
	TunnelToken string `json:"tunnelToken"`
	ConnectURL  string `json:"connectUrl"`
}

type tunnelMessage struct {
	Type    string              `json:"type"`
	ReqID   string              `json:"reqId,omitempty"`
	ConnID  string              `json:"connId,omitempty"`
	Method  string              `json:"method,omitempty"`
	Path    string              `json:"path,omitempty"`
	Query   string              `json:"query,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
	Status  int                 `json:"status,omitempty"`
	Binary  bool                `json:"binary,omitempty"`
	Stream  bool                `json:"stream,omitempty"`
	Error   string              `json:"error,omitempty"`
}

type agentRuntime struct {
	conn     *websocket.Conn
	send     chan tunnelMessage
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	haBaseURL *url.URL
	httpConn  *http.Client

	mu         sync.Mutex
	upstreamWS map[string]*upstreamWebSocket
	httpCancel map[string]context.CancelFunc
}

type upstreamWebSocket struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

type logLevel int

const (
	logLevelError logLevel = iota
	logLevelWarning
	logLevelInfo
	logLevelDebug
	logLevelTrace
)

var currentLogLevel = logLevelInfo

var strippedHeaders = map[string]struct{}{
	"x-forwarded-for":   {},
	"x-forwarded-host":  {},
	"x-forwarded-proto": {},
	"x-real-ip":         {},
	"forwarded":         {},
	"cf-connecting-ip":  {},
	"true-client-ip":    {},
}

func parseLogLevel(value string) logLevel {
	switch normalizeLogLevel(value) {
	case "trace":
		return logLevelTrace
	case "debug":
		return logLevelDebug
	case "warning", "warn":
		return logLevelWarning
	case "error":
		return logLevelError
	default:
		return logLevelInfo
	}
}

func normalizeLogLevel(value string) string {
	level := strings.TrimSpace(strings.ToLower(value))
	if level == "" {
		return "info"
	}
	if level == "warn" {
		return "warning"
	}
	return level
}

func logf(level logLevel, label, format string, args ...any) {
	if currentLogLevel < level {
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] %s\n", label, fmt.Sprintf(format, args...))
}

func infof(format string, args ...any) {
	logf(logLevelInfo, "info", format, args...)
}

func debugf(format string, args ...any) {
	logf(logLevelDebug, "debug", format, args...)
}

func tracef(format string, args ...any) {
	logf(logLevelTrace, "trace", format, args...)
}

func redactURLToken(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "<invalid-url>"
	}
	query := parsed.Query()
	if query.Has("token") {
		query.Set("token", "redacted")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

func main() {
	pairingCode := flag.String("pairing-code", "", "Pairing code from dashboard")
	edgeURL := flag.String("edge-url", "https://api.home.ctech.media", "Edge base URL")
	pairAPI := flag.String("pair-api", "https://home.ctech.media/api/agent/pair", "Pair endpoint URL")
	haBase := flag.String("ha-base-url", "http://homeassistant:8123", "Local Home Assistant base URL")
	logLevelFlag := flag.String("log-level", "info", "Log level: trace, debug, info, warning, error")
	tokenFile := flag.String("token-file", "/data/tunnel-token.json", "Persisted tunnel token file")
	flag.Parse()
	currentLogLevel = parseLogLevel(*logLevelFlag)
	infof("log level set to %s", normalizeLogLevel(*logLevelFlag))

	if strings.TrimSpace(*edgeURL) == "" {
		fmt.Fprintln(os.Stderr, "edge-url is required")
		os.Exit(1)
	}
	if strings.TrimSpace(*haBase) == "" {
		fmt.Fprintln(os.Stderr, "ha-base-url is required")
		os.Exit(1)
	}

	resp, err := initialPairResponse(*tokenFile, *pairAPI, *pairingCode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}

	for resp == nil {
		if strings.TrimSpace(*pairingCode) == "" {
			fmt.Fprintln(os.Stderr, "pairing-code is required because no saved tunnel token exists")
			time.Sleep(10 * time.Second)
			continue
		}

		fmt.Fprintf(os.Stderr, "pairing with %s\n", *pairAPI)
		nextResp, err := pair(*pairAPI, *pairingCode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pairing failed: %v; retrying in 10s\n", err)
			time.Sleep(10 * time.Second)
			continue
		}
		resp = nextResp
		if err := savePairResponse(*tokenFile, resp); err != nil {
			fmt.Fprintf(os.Stderr, "unable to save tunnel token: %v\n", err)
		}
		fmt.Fprintln(os.Stderr, "pairing succeeded; opening tunnel")
	}

	for {
		if err := runTunnel(resp, *edgeURL, *haBase); err != nil {
			fmt.Fprintf(os.Stderr, "tunnel disconnected: %v\n", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func initialPairResponse(tokenFile, pairAPI, pairingCode string) (*pairResponse, error) {
	if strings.TrimSpace(pairingCode) != "" {
		fmt.Fprintf(os.Stderr, "pairing with %s\n", pairAPI)
		resp, err := pair(pairAPI, pairingCode)
		if err == nil {
			if saveErr := savePairResponse(tokenFile, resp); saveErr != nil {
				fmt.Fprintf(os.Stderr, "unable to save tunnel token: %v\n", saveErr)
			}
			fmt.Fprintln(os.Stderr, "pairing succeeded; saved tunnel token")
			return resp, nil
		}
		fmt.Fprintf(os.Stderr, "pairing failed: %v\n", err)
	}

	resp, err := loadPairResponse(tokenFile)
	if err == nil && strings.TrimSpace(resp.TunnelToken) != "" {
		fmt.Fprintf(os.Stderr, "using saved tunnel token from %s\n", tokenFile)
		return resp, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("unable to read saved tunnel token: %v", err)
	}
	return nil, nil
}

func loadPairResponse(path string) (*pairResponse, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var resp pairResponse
	if err := json.Unmarshal(content, &resp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.TunnelToken) == "" {
		return nil, fmt.Errorf("saved token is empty")
	}
	return &resp, nil
}

func savePairResponse(path string, resp *pairResponse) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	content, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0600)
}

func runTunnel(resp *pairResponse, edgeURL, haBase string) error {
	connectURL := strings.TrimSpace(edgeURL)
	if connectURL != "" {
		connectURL = strings.TrimRight(edgeURL, "/") + "/_haremote/connect"
	} else {
		connectURL = strings.TrimSpace(resp.ConnectURL)
	}

	parsedConnect, err := url.Parse(connectURL)
	if err != nil {
		return err
	}
	query := parsedConnect.Query()
	query.Set("token", resp.TunnelToken)
	parsedConnect.RawQuery = query.Encode()

	wsURL := toWebSocketURL(parsedConnect)
	debugf("opening tunnel websocket to %s", redactURLToken(wsURL.String()))

	parsedHaBase, err := url.Parse(haBase)
	if err != nil {
		return err
	}
	debugf("forwarding local Home Assistant requests to %s", parsedHaBase.Redacted())

	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	conn, _, err := dialer.Dial(wsURL.String(), nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	infof("tunnel connected")

	runtime := &agentRuntime{
		conn:       conn,
		send:       make(chan tunnelMessage, 256),
		done:       make(chan struct{}),
		haBaseURL:  parsedHaBase,
		httpConn:   &http.Client{},
		upstreamWS: make(map[string]*upstreamWebSocket),
		httpCancel: make(map[string]context.CancelFunc),
	}

	go runtime.writePump()
	err = runtime.readPump()
	runtime.stop()
	runtime.wg.Wait()
	close(runtime.send)
	return err
}

func pair(url, code string) (*pairResponse, error) {
	payload := map[string]string{"pairingCode": code}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 {
		return nil, fmt.Errorf("pair api status %d: %s", response.StatusCode, string(responseBody))
	}

	var parsed pairResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (a *agentRuntime) readPump() error {
	for {
		var msg tunnelMessage
		if err := a.conn.ReadJSON(&msg); err != nil {
			return err
		}
		debugf("received tunnel message type=%s req=%s conn=%s path=%s stream=%t", msg.Type, msg.ReqID, msg.ConnID, msg.Path, msg.Stream)

		switch msg.Type {
		case "http_req":
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				a.handleHTTP(msg)
			}()
		case "http_cancel":
			a.cancelHTTP(msg.ReqID)
		case "ws_open":
			a.handleWSOpen(msg)
		case "ws_data":
			a.handleWSData(msg)
		case "ws_close":
			a.handleWSClose(msg.ConnID)
		}
	}
}

func (a *agentRuntime) writePump() {
	for msg := range a.send {
		a.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err := a.conn.WriteJSON(msg); err != nil {
			a.stop()
			return
		}
	}
}

func (a *agentRuntime) handleHTTP(msg tunnelMessage) {
	targetURL := *a.haBaseURL
	targetURL.Path = joinPath(a.haBaseURL.Path, msg.Path)
	targetURL.RawQuery = msg.Query
	debugf("http request start req=%s method=%s path=%s stream=%t", msg.ReqID, msg.Method, msg.Path, msg.Stream)

	body, err := base64.RawStdEncoding.DecodeString(msg.Body)
	if err != nil {
		a.replyError(msg.ReqID, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	ctx := context.Background()
	var cancel context.CancelFunc
	if msg.Stream {
		ctx, cancel = context.WithCancel(ctx)
		a.mu.Lock()
		a.httpCancel[msg.ReqID] = cancel
		a.mu.Unlock()
		defer a.cancelHTTP(msg.ReqID)
	}

	req, err := http.NewRequestWithContext(ctx, msg.Method, targetURL.String(), bytes.NewReader(body))
	if err != nil {
		a.replyError(msg.ReqID, fmt.Sprintf("request build failed: %v", err))
		return
	}

	for key, values := range msg.Headers {
		lower := strings.ToLower(key)
		if _, stripped := strippedHeaders[lower]; stripped {
			continue
		}
		if isHopByHopHeader(lower) {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	req.Host = a.haBaseURL.Host

	resp, err := a.httpConn.Do(req)
	if err != nil {
		a.replyError(msg.ReqID, fmt.Sprintf("upstream http failed: %v", err))
		return
	}
	defer resp.Body.Close()
	debugf("http upstream response req=%s status=%d path=%s", msg.ReqID, resp.StatusCode, msg.Path)

	if msg.Stream {
		a.streamHTTPResponse(msg.ReqID, resp)
		return
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		a.replyError(msg.ReqID, fmt.Sprintf("read upstream response failed: %v", err))
		return
	}

	a.sendMessage(tunnelMessage{
		Type:    "http_res",
		ReqID:   msg.ReqID,
		Status:  resp.StatusCode,
		Headers: cloneHeaders(resp.Header),
		Body:    base64.RawStdEncoding.EncodeToString(responseBody),
	})
	debugf("http response sent req=%s status=%d bytes=%d", msg.ReqID, resp.StatusCode, len(responseBody))
}

func (a *agentRuntime) streamHTTPResponse(reqID string, resp *http.Response) {
	if !a.sendMessage(tunnelMessage{
		Type:    "http_res",
		ReqID:   reqID,
		Status:  resp.StatusCode,
		Headers: cloneHeaders(resp.Header),
	}) {
		return
	}

	buffer := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			tracef("stream chunk req=%s bytes=%d", reqID, n)
			if !a.sendMessage(tunnelMessage{
				Type:  "http_chunk",
				ReqID: reqID,
				Body:  base64.RawStdEncoding.EncodeToString(chunk),
			}) {
				return
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				a.sendMessage(tunnelMessage{Type: "http_end", ReqID: reqID})
				debugf("stream ended req=%s", reqID)
			} else {
				a.sendMessage(tunnelMessage{Type: "http_end", ReqID: reqID, Error: readErr.Error()})
				debugf("stream ended with error req=%s error=%v", reqID, readErr)
			}
			return
		}
	}
}

func (a *agentRuntime) handleWSOpen(msg tunnelMessage) {
	upstreamURL := toUpstreamWSURL(a.haBaseURL, msg.Path, msg.Query)

	header := http.Header{}
	for key, values := range msg.Headers {
		lower := strings.ToLower(key)
		if _, stripped := strippedHeaders[lower]; stripped {
			continue
		}
		if isHopByHopHeader(lower) {
			continue
		}
		if isWebSocketHandshakeHeader(lower) {
			continue
		}
		for _, value := range values {
			header.Add(key, value)
		}
	}

	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	conn, response, err := dialer.Dial(upstreamURL.String(), header)
	if err != nil {
		status := ""
		if response != nil {
			status = response.Status
		}
		fmt.Fprintf(os.Stderr, "upstream websocket open failed path=%s status=%s error=%v\n", upstreamURL.Path, status, err)
		a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: msg.ConnID, Error: err.Error()})
		return
	}
	debugf("upstream websocket opened conn=%s path=%s", msg.ConnID, upstreamURL.Path)

	a.mu.Lock()
	a.upstreamWS[msg.ConnID] = &upstreamWebSocket{conn: conn}
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.readUpstreamWS(msg.ConnID, conn)
	}()
}

func (a *agentRuntime) handleWSData(msg tunnelMessage) {
	a.mu.Lock()
	upstream := a.upstreamWS[msg.ConnID]
	a.mu.Unlock()
	if upstream == nil {
		return
	}

	decoded, err := base64.RawStdEncoding.DecodeString(msg.Body)
	if err != nil {
		return
	}

	messageType := websocket.TextMessage
	if msg.Binary {
		messageType = websocket.BinaryMessage
	}
	upstream.writeMu.Lock()
	upstream.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	err = upstream.conn.WriteMessage(messageType, decoded)
	upstream.writeMu.Unlock()
	if err != nil {
		a.handleWSClose(msg.ConnID)
		debugf("upstream websocket write failed conn=%s error=%v", msg.ConnID, err)
	}
}

func (a *agentRuntime) readUpstreamWS(connID string, conn *websocket.Conn) {
	defer a.handleWSClose(connID)

	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: connID})
			return
		}

		if !a.sendMessage(tunnelMessage{
			Type:   "ws_data",
			ConnID: connID,
			Binary: messageType == websocket.BinaryMessage,
			Body:   base64.RawStdEncoding.EncodeToString(data),
		}) {
			return
		}
		tracef("upstream websocket frame conn=%s bytes=%d binary=%t", connID, len(data), messageType == websocket.BinaryMessage)
	}
}

func (a *agentRuntime) handleWSClose(connID string) {
	a.mu.Lock()
	upstream := a.upstreamWS[connID]
	delete(a.upstreamWS, connID)
	a.mu.Unlock()
	if upstream != nil {
		_ = upstream.conn.Close()
		debugf("upstream websocket closed conn=%s", connID)
	}
}

func (a *agentRuntime) closeUpstreams() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, upstream := range a.upstreamWS {
		_ = upstream.conn.Close()
		delete(a.upstreamWS, key)
	}
	for key, cancel := range a.httpCancel {
		cancel()
		delete(a.httpCancel, key)
	}
}

func (a *agentRuntime) replyError(reqID, message string) {
	a.sendMessage(tunnelMessage{Type: "http_res", ReqID: reqID, Status: http.StatusBadGateway, Error: message})
}

func (a *agentRuntime) cancelHTTP(reqID string) {
	a.mu.Lock()
	cancel := a.httpCancel[reqID]
	delete(a.httpCancel, reqID)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *agentRuntime) stop() {
	a.stopOnce.Do(func() {
		close(a.done)
		a.closeUpstreams()
		_ = a.conn.Close()
	})
}

func (a *agentRuntime) sendMessage(msg tunnelMessage) bool {
	select {
	case <-a.done:
		return false
	case a.send <- msg:
		return true
	}
}

func cloneHeaders(input http.Header) map[string][]string {
	result := make(map[string][]string, len(input))
	for key, values := range input {
		copied := make([]string, 0, len(values))
		for _, value := range values {
			copied = append(copied, value)
		}
		result[key] = copied
	}
	return result
}

func isHopByHopHeader(lower string) bool {
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func isWebSocketHandshakeHeader(lower string) bool {
	return lower == "host" ||
		lower == "origin" ||
		strings.HasPrefix(lower, "sec-websocket-")
}

func joinPath(basePath, requestPath string) string {
	basePath = strings.TrimSuffix(basePath, "/")
	if requestPath == "" || requestPath == "/" {
		if basePath == "" {
			return "/"
		}
		return basePath
	}
	if strings.HasPrefix(requestPath, "/") {
		return basePath + requestPath
	}
	return basePath + "/" + requestPath
}

func toWebSocketURL(parsed *url.URL) *url.URL {
	converted := *parsed
	if strings.EqualFold(converted.Scheme, "https") {
		converted.Scheme = "wss"
	} else {
		converted.Scheme = "ws"
	}
	return &converted
}

func toUpstreamWSURL(base *url.URL, path, query string) *url.URL {
	parsed := *base
	parsed.Path = joinPath(base.Path, path)
	parsed.RawQuery = query
	if strings.EqualFold(parsed.Scheme, "https") {
		parsed.Scheme = "wss"
	} else {
		parsed.Scheme = "ws"
	}
	return &parsed
}
