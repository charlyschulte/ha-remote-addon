package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func newUpstreamClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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
	query.Set("version", agentVersion)
	query.Set("caps", agentCapabilities())
	parsedConnect.RawQuery = query.Encode()

	wsURL := toWebSocketURL(parsedConnect)
	debugf("opening tunnel websocket to %s", redactURLToken(wsURL.String()))

	parsedHaBase, err := url.Parse(haBase)
	if err != nil {
		return err
	}
	debugf("forwarding local Home Assistant requests to %s", parsedHaBase.Redacted())

	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	conn, response, err := dialer.Dial(wsURL.String(), nil)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return errTokenRejected
		}
		return err
	}
	defer conn.Close()
	activeTunnel.Store(conn)
	defer activeTunnel.CompareAndSwap(conn, nil)
	lastConnectedAt.Store(time.Now().UnixNano())
	infof("tunnel connected")

	runtime := &agentRuntime{
		conn:       conn,
		send:       make(chan tunnelMessage, 256),
		done:       make(chan struct{}),
		haBaseURL:  parsedHaBase,
		httpConn:   newUpstreamClient(),
		upstreamWS: make(map[string]*upstreamWebSocket),
		httpCancel: make(map[string]context.CancelFunc),
		httpAcks:   make(map[string]chan int64),
	}

	go runtime.writePump()
	go runtime.pingLoop()
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
	_ = a.conn.SetReadDeadline(time.Now().Add(pongWait))
	a.conn.SetPongHandler(func(string) error {
		return a.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	a.conn.SetPingHandler(func(data string) error {
		_ = a.conn.SetReadDeadline(time.Now().Add(pongWait))
		err := a.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
		if err == websocket.ErrCloseSent {
			return nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		return err
	})

	for {
		var msg tunnelMessage
		if err := a.conn.ReadJSON(&msg); err != nil {
			return err
		}
		_ = a.conn.SetReadDeadline(time.Now().Add(pongWait))
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
		case "http_ack":
			a.ackHTTP(msg.ReqID, msg.Ack)
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
		_ = a.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := a.conn.WriteJSON(msg); err != nil {
			a.stop()
			// Keep draining so senders blocked on a.send are released.
			for range a.send {
			}
			return
		}
	}
}

func (a *agentRuntime) pingLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.done:
			return
		case <-ticker.C:
			if err := a.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				a.stop()
				return
			}
		}
	}
}

// registerHTTP makes every request cancellable (browser gone, tunnel lost)
// and, for flow-controlled streams, sets up the acknowledgement channel.

func (a *agentRuntime) replyError(reqID, message string) {
	a.sendMessage(tunnelMessage{Type: "http_res", ReqID: reqID, Status: http.StatusBadGateway, Error: message})
}

func (a *agentRuntime) cancelHTTP(reqID string) {
	a.mu.Lock()
	cancel := a.httpCancel[reqID]
	delete(a.httpCancel, reqID)
	delete(a.httpAcks, reqID)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *agentRuntime) stop() {
	a.stopOnce.Do(func() {
		close(a.done)
		a.closeUpstreams()
		if a.conn != nil {
			_ = a.conn.Close()
		}
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
