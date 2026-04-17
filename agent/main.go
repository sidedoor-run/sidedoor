package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sidedoor <port> [--subdomain <name>]")
		os.Exit(1)
	}

	port := os.Args[1]
	subdomain := ""
	for i, arg := range os.Args {
		if arg == "--subdomain" && i+1 < len(os.Args) {
			subdomain = os.Args[i+1]
		}
	}

	relayURL := os.Getenv("SIDEDOOR_RELAY")
	if relayURL == "" {
		relayURL = "wss://sidedoor.run/sidedoor/connect"
	}

	connect(relayURL, port, subdomain, 0)
}

func connect(relayURL, port, subdomain string, attempt int) {
	if attempt == 0 {
		fmt.Println("\n  sidedoor connecting...\n")
	}

	ctx := context.Background()

	wsConn, _, err := websocket.Dial(ctx, relayURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		backoff(attempt)
		connect(relayURL, port, subdomain, attempt+1)
		return
	}

	netConn := newWSNetConn(ctx, wsConn)

	if _, err := netConn.Write([]byte(subdomain + "\n")); err != nil {
		backoff(attempt)
		connect(relayURL, port, subdomain, attempt+1)
		return
	}

	buf := make([]byte, 512)
	n, err := netConn.Read(buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  relay error: %v\n", err)
		backoff(attempt)
		connect(relayURL, port, subdomain, attempt+1)
		return
	}
	msg := strings.TrimSpace(string(buf[:n]))

	cfg := yamux.DefaultConfig()
	cfg.MaxStreamWindowSize = 16 * 1024 * 1024
	cfg.LogOutput = io.Discard
	session, err := yamux.Client(netConn, cfg)
	if err != nil {
		backoff(attempt)
		connect(relayURL, port, subdomain, attempt+1)
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
	connect(relayURL, port, subdomain, attempt+1)
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
