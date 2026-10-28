package main

import (
	"net/http"
	"net/url"
	"strings"
)

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

func firstHeaderValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

func validatedOriginalHost(candidate, fallback string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || strings.ContainsAny(candidate, "\r\n/\\") {
		return fallback
	}
	if _, err := url.Parse("http://" + candidate); err != nil {
		return fallback
	}
	return candidate
}

func validatedOriginalProto(candidate string) string {
	candidate = strings.TrimSpace(strings.ToLower(candidate))
	if candidate == "http" || candidate == "https" {
		return candidate
	}
	return "http"
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

// setUpstreamPath points target at the request path below base. When the
// proxy sends the escaped path, encoded characters such as %2F survive.
func setUpstreamPath(target, base *url.URL, path, rawPath string) {
	target.Path = joinPath(base.Path, path)
	target.RawPath = ""
	if rawPath == "" {
		return
	}
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return
	}
	target.Path = joinPath(base.Path, decoded)
	target.RawPath = joinPath(base.EscapedPath(), rawPath)
}
