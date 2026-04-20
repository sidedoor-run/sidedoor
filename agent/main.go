package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

type wsNetConn struct {
	ctx      context.Context
	conn     *websocket.Conn
	reader   io.Reader
	replaced bool
}

func newWSNetConn(ctx context.Context, c *websocket.Conn) *wsNetConn {
	return &wsNetConn{ctx: ctx, conn: c}
}

func (c *wsNetConn) Read(b []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if err == io.EOF {
				c.reader = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		_, r, err := c.conn.Reader(c.ctx)
		if err != nil {
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Code == websocket.StatusGoingAway && closeErr.Reason == "replaced" {
				c.replaced = true
			}
			return 0, err
		}
		c.reader = r
	}
}

func (c *wsNetConn) Write(b []byte) (int, error) {
	if err := c.conn.Write(c.ctx, websocket.MessageBinary, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *wsNetConn) Close() error                       { return c.conn.Close(websocket.StatusNormalClosure, "") }
func (c *wsNetConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *wsNetConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *wsNetConn) SetDeadline(_ time.Time) error      { return nil }
func (c *wsNetConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *wsNetConn) SetWriteDeadline(_ time.Time) error { return nil }

func tokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sidedoor", "token")
}

func loadToken() string {
	b, err := os.ReadFile(tokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func saveToken(token string) error {
	path := tokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token), 0600)
}

func authURL() string {
	if u := os.Getenv("SIDEDOOR_AUTH_URL"); u != "" {
		return u
	}
	return "https://sidedoor-eight.vercel.app"
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func runAuth() {
	base := authURL()

	resp, err := http.PostForm(base+"/api/oauth/device/code", url.Values{
		"client_id": {"sidedoor-cli"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n  error reaching auth server: %v\n\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var code struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &code); err != nil {
		fmt.Fprintf(os.Stderr, "\n  error: %v\n  response: %s\n\n", err, string(body))
		os.Exit(1)
	}

	openBrowser(code.VerificationURI)
	fmt.Printf("\n  If the browser didn't open, visit:\n    %s\n\n", code.VerificationURI)
	fmt.Printf("  Enter code: %s\n\n", code.UserCode)

	interval := code.Interval
	if interval == 0 {
		interval = 5
	}

	for {
		time.Sleep(time.Duration(interval) * time.Second)

		r, err := http.PostForm(base+"/api/oauth/device/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {code.DeviceCode},
			"client_id":   {"sidedoor-cli"},
		})
		if err != nil {
			continue
		}

		var result struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		json.NewDecoder(r.Body).Decode(&result)
		r.Body.Close()

		if result.AccessToken != "" {
			if err := saveToken(result.AccessToken); err != nil {
				fmt.Fprintf(os.Stderr, "\n  error saving token: %v\n\n", err)
				os.Exit(1)
			}
			fmt.Printf("  Authenticated. You're all set.\n\n")
			return
		}

		if result.Error == "access_denied" || result.Error == "expired_token" {
			fmt.Fprintf(os.Stderr, "\n  auth failed: %s\n\n", result.Error)
			os.Exit(1)
		}
	}
}

func runLogout() {
	path := tokenPath()
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("  not logged in")
			return
		}
		fmt.Fprintf(os.Stderr, "  error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  logged out")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sidedoor <port>")
		fmt.Fprintln(os.Stderr, "       sidedoor auth")
		fmt.Fprintln(os.Stderr, "       sidedoor logout")
		os.Exit(1)
	}

	if os.Args[1] == "auth" {
		runAuth()
		return
	}

	if os.Args[1] == "logout" {
		runLogout()
		return
	}

	port := os.Args[1]
	if _, err := fmt.Sscanf(port, "%d", new(int)); err != nil {
		fmt.Fprintf(os.Stderr, "  error: '%s' is not a valid port number\n", port)
		fmt.Fprintln(os.Stderr, "  usage: sidedoor <port>")
		os.Exit(1)
	}

	relayURL := os.Getenv("SIDEDOOR_RELAY")
	if relayURL == "" {
		relayURL = "wss://sidedoor.run/sidedoor/connect"
	}

	token := loadToken()
	if token == "" {
		fmt.Fprintf(os.Stderr, "\n  not authenticated — run: sidedoor auth\n\n")
		os.Exit(1)
	}
	fmt.Println("\n  sidedoor connecting...\n")
	pinnedMachine := ""
	consecutiveFailures := 0
	for {
		nextMachine, err := tryConnect(relayURL, port, token, pinnedMachine)

		if nextMachine == "replaced" {
			fmt.Fprintf(os.Stderr, "\n  another sidedoor session started for this account — stopping\n\n")
			os.Exit(0)
		}

		if err != nil {
			if err.Error() == "fatal" {
				os.Exit(1)
			}
			// Failed to connect — back off
			consecutiveFailures++
		} else {
			// Was connected (dropped cleanly or health check) — reconnect immediately
			consecutiveFailures = 0
		}

		if nextMachine == "reset" {
			pinnedMachine = ""
		} else if nextMachine != "" {
			pinnedMachine = nextMachine
		}

		if consecutiveFailures == 0 {
			fmt.Printf("\n  reconnecting...\n\n")
		} else {
			delay := backoffDuration(consecutiveFailures)
			fmt.Printf("\n  reconnecting in %.0fs...\n\n", delay.Seconds())
			time.Sleep(delay)
		}
	}
}

func tryConnect(relayURL, port, token, pinnedMachine string) (string, error) {
	ctx := context.Background()

	dialOpts := &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	}
	if pinnedMachine != "" {
		dialOpts.HTTPHeader = http.Header{
			"Fly-Force-Instance-Id": {pinnedMachine},
		}
	}
	wsConn, _, err := websocket.Dial(ctx, relayURL, dialOpts)
	if err != nil {
		return "", err
	}

	netConn := newWSNetConn(ctx, wsConn)

	handshake := "\n"
	if token != "" {
		handshake = "token:" + token + "\n"
	}
	if _, err := netConn.Write([]byte(handshake)); err != nil {
		wsConn.Close(websocket.StatusAbnormalClosure, "")
		return "", err
	}

	buf := make([]byte, 512)
	n, err := netConn.Read(buf)
	if err != nil {
		wsConn.Close(websocket.StatusAbnormalClosure, "")
		return "", err
	}
	msg := strings.TrimSpace(string(buf[:n]))

	if strings.HasPrefix(msg, "error:") {
		fmt.Fprintf(os.Stderr, "  error: %s\n", strings.TrimPrefix(msg, "error:"))
		wsConn.Close(websocket.StatusNormalClosure, "")
		return "", fmt.Errorf("fatal")
	}

	cfg := yamux.DefaultConfig()
	cfg.MaxStreamWindowSize = 16 * 1024 * 1024
	cfg.LogOutput = io.Discard
	session, err := yamux.Client(netConn, cfg)
	if err != nil {
		wsConn.Close(websocket.StatusAbnormalClosure, "")
		return "", err
	}
	defer session.Close()

	parts := strings.SplitN(strings.TrimPrefix(msg, "ok:"), "|", 2)
	publicURL := parts[0]
	machineID := ""
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "machine:") {
			machineID = strings.TrimPrefix(p, "machine:")
		}
	}

	fmt.Printf("  Local    http://localhost:%s\n", port)
	fmt.Printf("  Public   %s\n", publicURL)
	fmt.Println("\n  ctrl+c to stop\n")

	// WebSocket keepalive
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := wsConn.Ping(ctx); err != nil {
					return
				}
			}
		}
	}()

	// Health check — verifies the full routing path end-to-end every 30s.
	// Catches stale Redis entries and cross-region routing failures that are
	// invisible to the agent (WebSocket alive but HTTP requests silently failing).
	healthFailed := make(chan struct{}, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		time.Sleep(10 * time.Second) // let things settle after connect
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			req, err := http.NewRequest("GET", publicURL, nil)
			if err != nil {
				continue
			}
			req.Header.Set("X-Sidedoor-Health", "1")
			resp, err := client.Do(req)
			if err != nil {
				// Network-level failure — WebSocket keepalive will handle this
				continue
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				fmt.Fprintf(os.Stderr, "\n  tunnel broken — reconnecting...\n")
				healthFailed <- struct{}{}
				session.Close() // unblocks session.Accept() below
				return
			}
		}
	}()

	for {
		stream, err := session.Accept()
		if err != nil {
			break
		}
		go handleStream(stream, port)
	}

	// If health check caused the disconnect, signal caller to clear machine pin
	select {
	case <-healthFailed:
		return "reset", nil
	default:
	}
	if netConn.replaced {
		return "replaced", nil
	}
	return machineID, nil
}

func handleStream(stream net.Conn, port string) {
	defer stream.Close()

	local, err := net.DialTimeout("tcp", "localhost:"+port, 5*time.Second)
	if err != nil {
		log.Printf("local dial error: %v", err)
		fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: 26\r\n\r\nlocal server not responding")
		return
	}
	defer local.Close()

	buf1 := make([]byte, 64*1024)
	buf2 := make([]byte, 64*1024)
	done := make(chan struct{}, 2)
	go func() { io.CopyBuffer(local, stream, buf1); done <- struct{}{} }()
	go func() { io.CopyBuffer(stream, local, buf2); done <- struct{}{} }()
	<-done
}

func backoffDuration(attempt int) time.Duration {
	d := time.Duration(1<<min(attempt, 5)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
