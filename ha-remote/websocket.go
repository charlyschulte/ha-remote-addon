package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func (a *agentRuntime) handleWSOpen(msg tunnelMessage) {
	upstream := &upstreamWebSocket{
		connID: msg.ConnID,
		out:    make(chan tunnelMessage, upstreamWSQueue),
		done:   make(chan struct{}),
	}
	a.mu.Lock()
	if a.upstreamWS == nil {
		a.upstreamWS = make(map[string]*upstreamWebSocket)
	}
	a.upstreamWS[msg.ConnID] = upstream
	a.mu.Unlock()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runUpstreamWS(msg, upstream)
	}()
}

// runUpstreamWS dials Home Assistant and then pumps queued browser frames
// to it in order. It runs outside the tunnel read loop, so a slow dial or
// a slow write only affects this one websocket.
func (a *agentRuntime) runUpstreamWS(msg tunnelMessage, upstream *upstreamWebSocket) {
	defer a.handleWSClose(msg.ConnID)

	base, _, custom, err := a.upstreamFor(msg.Target)
	if err != nil {
		warnf("websocket to custom target refused: %v", err)
		a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: msg.ConnID, Error: err.Error()})
		return
	}
	upstreamURL := toUpstreamWSURL(base, msg.Path, msg.Query)
	setUpstreamPath(upstreamURL, base, msg.Path, msg.RawPath)

	header := http.Header{}
	for key, values := range msg.Headers {
		lower := strings.ToLower(key)
		if lower == strings.ToLower(originalHostHeader) ||
			lower == strings.ToLower(originalProtoHeader) {
			continue
		}
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
	if len(msg.Protocols) > 0 {
		header.Set("Sec-WebSocket-Protocol", strings.Join(msg.Protocols, ", "))
	}
	forwardedHost := validatedOriginalHost(firstHeaderValue(msg.Headers, originalHostHeader), base.Host)
	header.Set("X-Forwarded-Host", forwardedHost)
	header.Set("X-Forwarded-Proto", validatedOriginalProto(firstHeaderValue(msg.Headers, originalProtoHeader)))

	dialCtx, cancelDial := context.WithCancel(context.Background())
	go func() {
		select {
		case <-upstream.done:
			cancelDial()
		case <-dialCtx.Done():
		}
	}()
	dialer := &websocket.Dialer{HandshakeTimeout: 20 * time.Second}
	if custom {
		dialer = targetWebSocketDialer()
	}
	conn, response, err := dialer.DialContext(dialCtx, upstreamURL.String(), header)
	cancelDial()
	if err != nil {
		select {
		case <-upstream.done:
			// The browser left while we were still connecting.
			return
		default:
		}
		status := ""
		if response != nil {
			status = response.Status
		}
		warnf("%s websocket open failed path=%s status=%s: %v", upstreamLabel(custom), upstreamURL.Path, status, err)
		a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: msg.ConnID, Error: err.Error()})
		return
	}

	upstream.mu.Lock()
	upstream.conn = conn
	upstream.mu.Unlock()
	select {
	case <-upstream.done:
		_ = conn.Close()
		return
	default:
	}
	debugf("upstream websocket opened conn=%s path=%s", msg.ConnID, upstreamURL.Path)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.readUpstreamWS(msg.ConnID, conn, upstream)
	}()

	for {
		select {
		case <-upstream.done:
			return
		case <-a.done:
			return
		case frame := <-upstream.out:
			decoded, err := base64.RawStdEncoding.DecodeString(frame.Body)
			if err != nil {
				continue
			}
			messageType := websocket.TextMessage
			if frame.Binary {
				messageType = websocket.BinaryMessage
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(messageType, decoded); err != nil {
				debugf("upstream websocket write failed conn=%s error=%v", msg.ConnID, err)
				a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: msg.ConnID})
				return
			}
		}
	}
}

func (a *agentRuntime) handleWSData(msg tunnelMessage) {
	a.mu.Lock()
	upstream := a.upstreamWS[msg.ConnID]
	a.mu.Unlock()
	if upstream == nil {
		return
	}

	select {
	case upstream.out <- msg:
	case <-upstream.done:
	default:
		// Dropping a frame would corrupt the Home Assistant protocol; close
		// the socket so the frontend reconnects cleanly.
		warnf("closing websocket conn=%s: Home Assistant is not keeping up", msg.ConnID)
		a.handleWSClose(msg.ConnID)
		a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: msg.ConnID})
	}
}

func (a *agentRuntime) readUpstreamWS(connID string, conn *websocket.Conn, upstream *upstreamWebSocket) {
	defer a.handleWSClose(connID)

	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-upstream.done:
				// Closed from the browser side; the proxy already knows.
			default:
				a.sendMessage(tunnelMessage{Type: "ws_close", ConnID: connID})
			}
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
		upstream.close()
		debugf("upstream websocket closed conn=%s", connID)
	}
}

func (a *agentRuntime) closeUpstreams() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, upstream := range a.upstreamWS {
		upstream.close()
		delete(a.upstreamWS, key)
	}
	for key, cancel := range a.httpCancel {
		cancel()
		delete(a.httpCancel, key)
	}
	for key := range a.httpAcks {
		delete(a.httpAcks, key)
	}
}
