package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

type logLevel int

const (
	logLevelError logLevel = iota
	logLevelWarning
	logLevelInfo
	logLevelDebug
	logLevelTrace
)

var currentLogLevel = logLevelInfo

// activeTunnel is the current tunnel connection, closed cleanly on shutdown.

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

func warnf(format string, args ...any) {
	logf(logLevelWarning, "warning", format, args...)
}

func errorf(format string, args ...any) {
	logf(logLevelError, "error", format, args...)
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
