package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var activeTunnel atomic.Pointer[websocket.Conn]

// lastConnectedAt (unix nanos) lets the reconnect loop reset its backoff
// once a tunnel was established.
var lastConnectedAt atomic.Int64

// exitOnSignal stops the add-on promptly: as PID 1 in the container the agent
// would otherwise ignore SIGTERM and get killed after the stop timeout.
func exitOnSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	sig := <-signals
	infof("received %s; closing tunnel", sig)
	if conn := activeTunnel.Load(); conn != nil {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "agent shutting down"),
			time.Now().Add(2*time.Second),
		)
	}
	os.Exit(0)
}

func main() {
	pairingCode := flag.String("pairing-code", "", "Pairing code from dashboard")
	edgeURL := flag.String("edge-url", "https://api.home.ctech.media", "Edge base URL")
	pairAPI := flag.String("pair-api", "https://home.ctech.media/api/agent/pair", "Pair endpoint URL")
	haBase := flag.String("ha-base-url", "http://homeassistant:8123", "Local Home Assistant base URL")
	logLevelFlag := flag.String("log-level", "info", "Log level: trace, debug, info, warning, error")
	tokenFile := flag.String("token-file", "/data/tunnel-token.json", "Persisted tunnel token file")
	allowTargets := flag.Bool("allow-custom-targets", true, "Allow forwarding subdomains to other services in the local network")
	flag.Parse()
	currentLogLevel = parseLogLevel(*logLevelFlag)
	go exitOnSignal()
	allowCustomTargets = *allowTargets
	infof("agent version %s; custom targets %s", agentVersion, map[bool]string{true: "allowed", false: "disabled"}[allowCustomTargets])
	infof("log level set to %s", normalizeLogLevel(*logLevelFlag))

	if strings.TrimSpace(*edgeURL) == "" {
		errorf("edge-url is required")
		os.Exit(1)
	}
	if strings.TrimSpace(*haBase) == "" {
		errorf("ha-base-url is required")
		os.Exit(1)
	}

	resp, err := initialPairResponse(*tokenFile, *pairAPI, *pairingCode)
	if err != nil {
		errorf("%v", err)
	}

	for resp == nil {
		if strings.TrimSpace(*pairingCode) == "" {
			errorf("no saved tunnel token found: enter a pairing code from the HA Remote dashboard in the add-on configuration")
			time.Sleep(60 * time.Second)
			continue
		}

		infof("pairing with %s", *pairAPI)
		nextResp, err := pair(*pairAPI, *pairingCode)
		if err != nil {
			errorf("pairing failed: %v; retrying in 30s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		nextResp.PairingCodeHash = pairingCodeHash(*pairingCode)
		resp = nextResp
		if err := savePairResponse(*tokenFile, resp); err != nil {
			warnf("unable to save tunnel token: %v", err)
		}
		infof("pairing succeeded; opening tunnel")
	}

	delay := time.Second
	for {
		startedAt := time.Now()
		err := runTunnel(resp, *edgeURL, *haBase)
		switch {
		case errors.Is(err, errTokenRejected):
			errorf("the server rejected the saved tunnel token (instance deleted or re-paired); create a new pairing code in the HA Remote dashboard and enter it in the add-on configuration")
			time.Sleep(tokenRejectedDelay)
			continue
		case err != nil:
			warnf("tunnel disconnected: %s", describeTunnelError(err))
		}

		// After a working tunnel drops, reconnect quickly; repeated failed
		// dials back off so an outage does not flood the logs.
		if lastConnectedAt.Load() >= startedAt.UnixNano() {
			delay = time.Second
		}
		infof("reconnecting in %s", delay)
		time.Sleep(delay)
		delay *= 2
		if delay > maxReconnectDelay {
			delay = maxReconnectDelay
		}
	}
}

// describeTunnelError turns websocket/network errors into short readable
// reasons for the add-on log.
func describeTunnelError(err error) string {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway:
			return "server closed the connection (restart or deployment)"
		case websocket.CloseAbnormalClosure:
			return "connection to the server was lost"
		case websocket.ClosePolicyViolation:
			if closeErr.Text != "" {
				return closeErr.Text
			}
		}
		return fmt.Sprintf("server closed the connection (code %d)", closeErr.Code)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "no keepalive from the server; connection considered dead"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection to the server was lost"
	}
	return err.Error()
}

func initialPairResponse(tokenFile, pairAPI, pairingCode string) (*pairResponse, error) {
	pairingHash := pairingCodeHash(pairingCode)
	if pairingHash != "" {
		saved, err := loadPairResponse(tokenFile)
		if err == nil && saved.PairingCodeHash != "" && saved.PairingCodeHash != pairingHash {
			infof("pairing code changed; deleting saved tunnel token")
			if removeErr := os.Remove(tokenFile); removeErr != nil && !os.IsNotExist(removeErr) {
				warnf("unable to delete saved tunnel token: %v", removeErr)
			}
		} else if err == nil && saved.PairingCodeHash == pairingHash && strings.TrimSpace(saved.TunnelToken) != "" {
			infof("using saved tunnel token from %s", tokenFile)
			return saved, nil
		} else if err == nil && saved.PairingCodeHash == "" {
			infof("saved tunnel token has no pairing code metadata; pairing with configured code")
			if removeErr := os.Remove(tokenFile); removeErr != nil && !os.IsNotExist(removeErr) {
				warnf("unable to delete saved tunnel token: %v", removeErr)
			}
		} else if err != nil && !os.IsNotExist(err) {
			warnf("unable to read saved tunnel token: %v", err)
		}

		infof("pairing with %s", pairAPI)
		resp, err := pair(pairAPI, pairingCode)
		if err == nil {
			resp.PairingCodeHash = pairingHash
			if saveErr := savePairResponse(tokenFile, resp); saveErr != nil {
				warnf("unable to save tunnel token: %v", saveErr)
			}
			infof("pairing succeeded; saved tunnel token")
			return resp, nil
		}
		errorf("pairing failed: %v", err)
		return nil, nil
	}

	resp, err := loadPairResponse(tokenFile)
	if err == nil && strings.TrimSpace(resp.TunnelToken) != "" {
		infof("using saved tunnel token from %s", tokenFile)
		return resp, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("unable to read saved tunnel token: %v", err)
	}
	return nil, nil
}

func pairingCodeHash(pairingCode string) string {
	normalized := strings.TrimSpace(strings.ToUpper(pairingCode))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
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

// newUpstreamClient builds the HTTP client used to reach Home Assistant.
// Redirects must reach the browser unchanged (following them here would
// serve content under the wrong URL), and compression must stay untouched.

// agentVersion is reported to the server so the dashboard can show outdated
// add-ons. Keep it in sync with config.yaml.
const agentVersion = "0.2.0"

func agentCapabilities() string {
	capabilities := []string{"flow"}
	if allowCustomTargets {
		capabilities = append(capabilities, "targets")
	}
	return strings.Join(capabilities, ",")
}

func upstreamLabel(custom bool) string {
	if custom {
		return "custom target"
	}
	return "Home Assistant"
}
