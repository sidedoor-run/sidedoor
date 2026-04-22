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
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

var version = "dev"

// ---- Connection status (shared between connect loop and status server) ----

type Status struct {
	mu         sync.Mutex
	State      string
	URL        string
	Reconnects int
	Since      time.Time
}

func (s *Status) setState(state, url, tunnelPort string) {
	s.mu.Lock()
	if state == "running" && s.State != "running" && s.URL != "" {
		s.Reconnects++
	}
	s.State = state
	if url != "" {
		s.URL = url
	}
	snap := map[string]any{
		"state":      s.State,
		"url":        s.URL,
		"reconnects": s.Reconnects,
		"uptime_s":   time.Since(s.Since).Seconds(),
	}
	s.mu.Unlock()
	// Write to ~/.sidedoor/agent-<port>.json so the desktop can read it without polling.
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".sidedoor", "agent-"+tunnelPort+".json")
		if data, err := json.Marshal(snap); err == nil {
			os.WriteFile(path, data, 0644)
		}
	}
}

func (s *Status) snapshot() (state, surl string, reconnects int, uptimeSec float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State, s.URL, s.Reconnects, time.Since(s.Since).Seconds()
}

// ---- WebSocket net.Conn adapter ----

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

// ---- Auth helpers ----

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

// ---- Status HTTP server ----

func statusPort(tunnelPort string) int {
	p, _ := strconv.Atoi(tunnelPort)
	sp := p + 10000
	if sp > 65535 {
		sp = p - 1000
	}
	return sp
}

func serveStatus(tunnelPort string, status *Status) {
	sp := statusPort(tunnelPort)
	addr := fmt.Sprintf("localhost:%d", sp)

	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		state, surl, reconnects, uptime := status.snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]any{
			"state":      state,
			"url":        surl,
			"reconnects": reconnects,
			"uptime_s":   uptime,
		})
	})

	// Probes the tunnel end-to-end using the health-check shortcut so no local
	// server is needed. Returns latency and ok/fail for the dashboard history.
	mux.HandleFunc("/api/probe", func(w http.ResponseWriter, r *http.Request) {
		_, surl, _, _ := status.snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if surl == "" {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not connected"})
			return
		}
		req, _ := http.NewRequest("GET", surl, nil)
		req.Header.Set("X-Sidedoor-Health", "1")
		client := &http.Client{Timeout: 5 * time.Second}
		start := time.Now()
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "latency_ms": ms})
			return
		}
		resp.Body.Close()
		json.NewEncoder(w).Encode(map[string]any{"ok": resp.StatusCode == http.StatusOK, "latency_ms": ms})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(strings.ReplaceAll(statusHTML, "{{PORT}}", tunnelPort)))
	})

	go http.ListenAndServe(addr, mux)
	fmt.Printf("  Status   http://%s\n", addr)
}

var statusHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>sidedoor · port {{PORT}}</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#111;color:#e5e5e5;padding:40px}
h1{font-size:13px;font-weight:600;color:#555;letter-spacing:.08em;text-transform:uppercase;margin-bottom:24px}
.card{background:#1a1a1a;border:1px solid #2a2a2a;border-radius:12px;padding:24px;max-width:500px}
.row{display:flex;align-items:center;gap:10px;margin-bottom:14px}
.dot{width:10px;height:10px;border-radius:50%;flex-shrink:0;transition:background .3s}
.running{background:#22c55e;box-shadow:0 0 8px #22c55e66}
.reconnecting{background:#f97316;box-shadow:0 0 8px #f9731666}
.connecting{background:#eab308;box-shadow:0 0 8px #eab30866}
.stopped{background:#ef4444}
.state-label{font-size:17px;font-weight:600}
.url{font-family:'SF Mono','Fira Code',monospace;font-size:13px;margin-bottom:18px;min-height:18px}
.url a{color:#60a5fa;text-decoration:none}.url a:hover{text-decoration:underline}
.stats{display:flex;gap:28px;margin-bottom:20px}
.stat{font-size:12px;color:#555}
.stat strong{color:#ccc;display:block;font-size:22px;font-weight:700;line-height:1.2}
.hist-label{font-size:11px;color:#444;text-transform:uppercase;letter-spacing:.06em;margin-bottom:8px}
.history{display:flex;gap:2px;align-items:flex-end;height:28px}
.tick{width:5px;border-radius:2px;transition:background .2s}
.tok{background:#22c55e;height:100%}.tfail{background:#ef4444;height:55%}.tpend{background:#2a2a2a;height:30%}
</style>
</head>
<body>
<h1>sidedoor &middot; port {{PORT}}</h1>
<div class="card">
  <div class="row">
    <div class="dot" id="dot"></div>
    <div class="state-label" id="state">—</div>
  </div>
  <div class="url" id="url"></div>
  <div class="stats">
    <div class="stat"><strong id="uptime">—</strong>uptime</div>
    <div class="stat"><strong id="reconnects">—</strong>reconnects</div>
    <div class="stat"><strong id="latency">—</strong>latency</div>
  </div>
  <div class="hist-label">last 60 probes · 2s interval</div>
  <div class="history" id="history"></div>
</div>
<script>
const hist=Array(60).fill(null);
async function pollStatus(){
  try{
    const d=await fetch('/api/status').then(r=>r.json());
    const dot=document.getElementById('dot');
    dot.className='dot '+(d.state||'stopped');
    document.getElementById('state').textContent=d.state||'stopped';
    const u=document.getElementById('url');
    u.innerHTML=d.url?'<a href="'+d.url+'" target="_blank">'+d.url+'</a>':'';
    const s=Math.floor(d.uptime_s),m=Math.floor(s/60);
    document.getElementById('uptime').textContent=m>0?m+'m '+(s%60)+'s':s+'s';
    document.getElementById('reconnects').textContent=d.reconnects;
  }catch{}
}
async function pollProbe(){
  try{
    const d=await fetch('/api/probe').then(r=>r.json());
    hist.shift();hist.push(d.ok);
    document.getElementById('latency').textContent=d.ok?d.latency_ms+'ms':'—';
  }catch{hist.shift();hist.push(false);}
  document.getElementById('history').innerHTML=
    hist.map(h=>'<div class="tick '+(h===null?'tpend':h?'tok':'tfail')+'"></div>').join('');
}
setInterval(pollStatus,1000);
setInterval(pollProbe,2000);
pollStatus();pollProbe();
</script>
</body>
</html>`

// ---- Main ----

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sidedoor <port>")
		fmt.Fprintln(os.Stderr, "       sidedoor auth")
		fmt.Fprintln(os.Stderr, "       sidedoor logout")
		os.Exit(1)
	}

	if os.Args[1] == "--version" || os.Args[1] == "version" {
		fmt.Println(version)
		return
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

	status := &Status{State: "connecting", Since: time.Now()}
	serveStatus(port, status)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		if home, err := os.UserHomeDir(); err == nil {
			os.Remove(filepath.Join(home, ".sidedoor", "agent-"+port+".json"))
		}
		os.Exit(0)
	}()

	fmt.Print("\n  sidedoor connecting...\n\n")
	pinnedMachine := ""
	consecutiveFailures := 0
	for {
		status.setState("connecting", "", port)
		nextMachine, err := tryConnect(relayURL, port, token, pinnedMachine, status)

		if nextMachine == "replaced" {
			fmt.Fprintf(os.Stderr, "\n  another sidedoor session started for this account — stopping\n\n")
			os.Exit(0)
		}

		if err != nil {
			if err.Error() == "fatal" {
				os.Exit(1)
			}
			consecutiveFailures++
		} else {
			consecutiveFailures = 0
		}

		if nextMachine == "reset" {
			pinnedMachine = ""
		} else if nextMachine != "" {
			pinnedMachine = nextMachine
		}

		reconnectLabel := "reconnecting"
		if nextMachine == "reset" {
			reconnectLabel = "tunnel unreachable — reconnecting"
		}
		status.setState("reconnecting", "", port)
		if consecutiveFailures == 0 {
			fmt.Printf("\n  %s...\n\n", reconnectLabel)
		} else {
			delay := backoffDuration(consecutiveFailures)
			fmt.Printf("\n  %s in %.0fs...\n\n", reconnectLabel, delay.Seconds())
			time.Sleep(delay)
		}
	}
}

func tryConnect(relayURL, port, token, pinnedMachine string, status *Status) (string, error) {
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

	if _, err := netConn.Write([]byte("token:" + token + "\n")); err != nil {
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

	status.setState("running", publicURL, port)
	fmt.Printf("  Local    http://localhost:%s\n", port)
	fmt.Printf("  Public   %s\n", publicURL)
	fmt.Print("\n  ctrl+c to stop\n\n")

	// done is closed when tryConnect returns, stopping background goroutines.
	done := make(chan struct{})
	defer close(done)

	// WebSocket keepalive — closes the session on failure so Accept() unblocks immediately.
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := wsConn.Ping(ctx); err != nil {
					session.Close()
					return
				}
			}
		}
	}()

	// Health check — verifies the yamux layer then the relay routing path every 30s.
	healthFailed := make(chan struct{}, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		select {
		case <-done:
			return
		case <-time.After(10 * time.Second):
		}
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// Verify the yamux tunnel itself is still open.
				stream, err := session.Open()
				if err != nil {
					select {
					case healthFailed <- struct{}{}:
					default:
					}
					session.Close()
					return
				}
				stream.Close()
				// Verify relay still routes this subdomain to us.
				req, err := http.NewRequest("GET", publicURL, nil)
				if err != nil {
					continue
				}
				req.Header.Set("X-Sidedoor-Health", "1")
				resp, err := client.Do(req)
				if err != nil {
					continue
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					select {
					case healthFailed <- struct{}{}:
					default:
					}
					session.Close()
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
		fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: 27\r\n\r\nlocal server not responding")
		return
	}
	defer local.Close()

	buf1 := make([]byte, 64*1024)
	buf2 := make([]byte, 64*1024)
	done := make(chan struct{}, 2)
	go func() { io.CopyBuffer(local, stream, buf1); done <- struct{}{} }()
	go func() { io.CopyBuffer(stream, local, buf2); done <- struct{}{} }()
	<-done
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
