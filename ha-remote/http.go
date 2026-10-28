package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (a *agentRuntime) registerHTTP(reqID string, cancel context.CancelFunc, flow bool) chan int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.httpCancel == nil {
		a.httpCancel = make(map[string]context.CancelFunc)
	}
	a.httpCancel[reqID] = cancel
	if !flow {
		return nil
	}
	if a.httpAcks == nil {
		a.httpAcks = make(map[string]chan int64)
	}
	acks := make(chan int64, 256)
	a.httpAcks[reqID] = acks
	return acks
}

func (a *agentRuntime) handleHTTP(msg tunnelMessage) {
	base, client, custom, err := a.upstreamFor(msg.Target)
	if err != nil {
		a.replyError(msg.ReqID, err.Error())
		return
	}
	targetURL := *base
	setUpstreamPath(&targetURL, base, msg.Path, msg.RawPath)
	targetURL.RawQuery = msg.Query
	debugf("http request start req=%s method=%s path=%s stream=%t", msg.ReqID, msg.Method, msg.Path, msg.Stream)

	body, err := base64.RawStdEncoding.DecodeString(msg.Body)
	if err != nil {
		a.replyError(msg.ReqID, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if msg.Stream {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), nonStreamTimeout)
	}
	acks := a.registerHTTP(msg.ReqID, cancel, msg.Stream && msg.Flow)
	defer a.cancelHTTP(msg.ReqID)

	req, err := http.NewRequestWithContext(ctx, msg.Method, targetURL.String(), bytes.NewReader(body))
	if err != nil {
		a.replyError(msg.ReqID, fmt.Sprintf("request build failed: %v", err))
		return
	}

	originalHost := firstHeaderValue(msg.Headers, originalHostHeader)
	originalProto := validatedOriginalProto(firstHeaderValue(msg.Headers, originalProtoHeader))
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
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	forwardedHost := validatedOriginalHost(originalHost, base.Host)
	req.Host = forwardedHost
	if custom {
		// Services on the LAN expect their own address as Host (virtual
		// hosts, CSRF checks); the public name stays in X-Forwarded-Host.
		req.Host = base.Host
	}
	req.Header.Set("X-Forwarded-Host", forwardedHost)
	req.Header.Set("X-Forwarded-Proto", originalProto)

	resp, err := doUpstreamRequest(client, req)
	if err != nil {
		if ctx.Err() == context.Canceled {
			debugf("http request cancelled before Home Assistant answered req=%s path=%s", msg.ReqID, msg.Path)
			return
		}
		warnf("%s request failed method=%s path=%s: %v", upstreamLabel(custom), msg.Method, msg.Path, err)
		if !custom {
			homeAssistant.noticeFailure(err)
		}
		a.replyError(msg.ReqID, fmt.Sprintf("upstream http failed: %v", err))
		return
	}
	defer resp.Body.Close()
	debugf("http upstream response req=%s status=%d path=%s", msg.ReqID, resp.StatusCode, msg.Path)

	if msg.Stream {
		a.streamHTTPResponse(ctx, msg.ReqID, resp, acks)
		return
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		warnf("reading Home Assistant response failed path=%s: %v", msg.Path, err)
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

func doUpstreamRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err == nil || !isRetryableUpstreamRequest(req, err) {
		return resp, err
	}

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
	}
	debugf("retrying upstream request after connection closed method=%s path=%s error=%v", req.Method, req.URL.Path, err)
	return client.Do(retry)
}

func isRetryableUpstreamRequest(req *http.Request, err error) bool {
	if req == nil || err == nil {
		return false
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (a *agentRuntime) streamHTTPResponse(ctx context.Context, reqID string, resp *http.Response, acks chan int64) {
	if !a.sendMessage(tunnelMessage{
		Type:    "http_res",
		ReqID:   reqID,
		Status:  resp.StatusCode,
		Headers: cloneHeaders(resp.Header),
	}) {
		return
	}

	inFlight := int64(0)
	buffer := make([]byte, 32*1024)
	for {
		// With flow control, wait until the browser has received enough of
		// what was already sent; this keeps one slow download from flooding
		// the shared tunnel.
		for acks != nil && inFlight >= flowWindow {
			select {
			case acked := <-acks:
				inFlight -= acked
			case <-ctx.Done():
				return
			case <-a.done:
				return
			}
		}

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
			inFlight += int64(n)
		}
		if readErr != nil {
			switch {
			case readErr == io.EOF:
				a.sendMessage(tunnelMessage{Type: "http_end", ReqID: reqID})
				debugf("stream ended req=%s", reqID)
			case ctx.Err() != nil:
				// Cancelled by the proxy because the browser went away;
				// nobody is waiting for an answer.
				debugf("stream cancelled req=%s", reqID)
			default:
				a.sendMessage(tunnelMessage{Type: "http_end", ReqID: reqID, Error: readErr.Error()})
				debugf("stream ended with error req=%s error=%v", reqID, readErr)
			}
			return
		}
	}
}

func (a *agentRuntime) ackHTTP(reqID string, bytes int64) {
	a.mu.Lock()
	acks := a.httpAcks[reqID]
	a.mu.Unlock()
	if acks == nil || bytes <= 0 {
		return
	}
	select {
	case acks <- bytes:
	default:
		// 256 queued acks cover far more than the window; drain one and
		// fold it in so no credit is lost.
		select {
		case previous := <-acks:
			acks <- previous + bytes
		default:
		}
	}
}
