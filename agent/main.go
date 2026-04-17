package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

type wsNetConn struct {
	ctx    context.Context
	conn   *websocket.Conn
	reader io.Reader
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

func runAuth() {
	base := authURL()

	resp, err := http.PostForm(base+"/oauth/device/code", url.Values{
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

	fmt.Printf("\n  Open this URL in your browser:\n\n")
	fmt.Printf("    %s\n\n", code.VerificationURI)
	fmt.Printf("  Enter code: %s\n\n", code.UserCode)

	interval := code.Interval
	if interval == 0 {
		interval = 5
	}

	for {
		time.Sleep(time.Duration(interval) * time.Second)

		r, err := http.PostForm(base+"/oauth/device/token", url.Values{
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sidedoor <port>")
		fmt.Fprintln(os.Stderr, "       sidedoor auth")
		os.Exit(1)
	}

	if os.Args[1] == "auth" {
		runAuth()
		return
	}

	port := os.Args[1]

	relayURL := os.Getenv("SIDEDOOR_RELAY")
	if relayURL == "" {
		relayURL = "wss://sidedoor.run/sidedoor/connect"
	}

	token := loadToken()
	connect(relayURL, port, token, 0)
}

func connect(relayURL, port, token string, attempt int) {
	if attempt == 0 {
		fmt.Println("\n  sidedoor connecting...\n")
	}

	ctx := context.Background()

	wsConn, _, err := websocket.Dial(ctx, relayURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		backoff(attempt)
		connect(relayURL, port, token, attempt+1)
		return
	}

	netConn := newWSNetConn(ctx, wsConn)

	handshake := "\n"
	if token != "" {
		handshake = "token:" + token + "\n"
	}
	if _, err := netConn.Write([]byte(handshake)); err != nil {
		backoff(attempt)
		connect(relayURL, port, token, attempt+1)
		return
	}

	buf := make([]byte, 512)
	n, err := netConn.Read(buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  relay error: %v\n", err)
		backoff(attempt)
		connect(relayURL, port, token, attempt+1)
		return
	}
	msg := strings.TrimSpace(string(buf[:n]))

	cfg := yamux.DefaultConfig()
	cfg.MaxStreamWindowSize = 16 * 1024 * 1024
	cfg.LogOutput = io.Discard
	session, err := yamux.Client(netConn, cfg)
	if err != nil {
		backoff(attempt)
		connect(relayURL, port, token, attempt+1)
		return
	}
	defer session.Close()

	if strings.HasPrefix(msg, "error:") {
		fmt.Fprintf(os.Stderr, "  error: %s\n", strings.TrimPrefix(msg, "error:"))
		os.Exit(1)
	}

	publicURL := strings.TrimPrefix(msg, "ok:")
	fmt.Printf("  Local    http://localhost:%s\n", port)
	fmt.Printf("  Public   %s\n", publicURL)
	fmt.Println("\n  ctrl+c to stop\n")

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

	for {
		stream, err := session.Accept()
		if err != nil {
			break
		}
		go handleStream(stream, port)
	}

	delay := backoffDuration(attempt)
	fmt.Printf("\n  disconnected — reconnecting in %.0fs...\n\n", delay.Seconds())
	time.Sleep(delay)
	connect(relayURL, port, token, attempt+1)
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

func backoff(attempt int) {
	time.Sleep(backoffDuration(attempt))
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
