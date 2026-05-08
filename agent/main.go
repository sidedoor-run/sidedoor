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

var errFatal = errors.New("fatal")

// ---- Connection status (shared between connect loop and status server) ----

type Status struct {
	mu             sync.Mutex
	State          string
	URL            string
	Reconnects     int
	Since          time.Time
	LocalState     string
	LocalError     string
	LocalLatencyMS int64
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
	snap := s.snapshotLocked()
	s.mu.Unlock()
	writeStatusFile(tunnelPort, snap)
}

func (s *Status) setLocal(state, localErr string, latencyMS int64, tunnelPort string) {
	s.mu.Lock()
	s.LocalState = state
	s.LocalError = localErr
	s.LocalLatencyMS = latencyMS
	snap := s.snapshotLocked()
	s.mu.Unlock()
	writeStatusFile(tunnelPort, snap)
}

func (s *Status) snapshotLocked() map[string]any {
	localState := s.LocalState
	if localState == "" {
		localState = "unknown"
	}
	return map[string]any{
		"state":            s.State,
		"url":              s.URL,
		"reconnects":       s.Reconnects,
		"uptime_s":         time.Since(s.Since).Seconds(),
		"local_state":      localState,
		"local_error":      s.LocalError,
		"local_latency_ms": s.LocalLatencyMS,
	}
}

func (s *Status) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func writeStatusFile(tunnelPort string, snap map[string]any) {
	// Write to ~/.sidedoor/agent-<port>.json so the desktop can read it without polling.
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".sidedoor", "agent-"+tunnelPort+".json")
		if data, err := json.Marshal(snap); err == nil {
			os.WriteFile(path, data, 0644)
		}
	}
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

// keychainGet/Set/Delete use the macOS `security` CLI on darwin and
// `secret-tool` on Linux. Both work without CGo or entitlements.
// On unsupported platforms the token falls back to the legacy file.

func keychainGet() (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("security", "find-generic-password", "-s", "sidedoor", "-a", "token", "-w")
	case "linux":
		cmd = exec.Command("secret-tool", "lookup", "service", "sidedoor", "account", "token")
	default:
		return "", fmt.Errorf("unsupported")
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func keychainSet(token string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("security", "add-generic-password", "-U", "-s", "sidedoor", "-a", "token", "-w", token)
	case "linux":
		cmd = exec.Command("secret-tool", "store", "--label=sidedoor token", "service", "sidedoor", "account", "token")
		cmd.Stdin = strings.NewReader(token)
	default:
		return fmt.Errorf("unsupported")
	}
	return cmd.Run()
}

func keychainDelete() error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("security", "delete-generic-password", "-s", "sidedoor", "-a", "token")
	case "linux":
		cmd = exec.Command("secret-tool", "clear", "service", "sidedoor", "account", "token")
	default:
		return fmt.Errorf("unsupported")
	}
	return cmd.Run()
}

func legacyTokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sidedoor", "token")
}

func loadToken() string {
	if t := os.Getenv("SIDEDOOR_TOKEN"); t != "" {
		return t
	}
	// Migrate plaintext file to keychain on first use.
	if path := legacyTokenPath(); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			token := strings.TrimSpace(string(b))
			if token != "" {
				if keychainSet(token) == nil {
					_ = os.Remove(path)
				}
				return token
			}
		}
	}
	token, err := keychainGet()
	if err != nil {
		return ""
	}
	return token
}

func saveToken(token string) error {
	return keychainSet(token)
}

func checkCLIVersion() (latest, required string) {
	base := authURL()
	req, err := http.NewRequest("GET", base+"/api/cli/version", nil)
	if err != nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return "", ""
	}
	defer resp.Body.Close()
	var result struct {
		Version  string `json:"version"`
		Required string `json:"required"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", ""
	}
	return result.Version, result.Required
}

func versionLessThan(a, b string) bool {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var ai, bi int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &ai)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &bi)
		}
		if ai != bi {
			return ai < bi
		}
	}
	return false
}

func touchAuthSentinel() {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".sidedoor", ".auth")
	os.WriteFile(path, nil, 0600)
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
			touchAuthSentinel()
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
	err := keychainDelete()
	_ = os.Remove(legacyTokenPath())
	touchAuthSentinel()
	if err != nil {
		fmt.Println("  not logged in")
		return
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

func probeLocalHTTP(port string, timeout time.Duration) (state, message string, latencyMS int64) {
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	start := time.Now()
	resp, err := client.Get("http://localhost:" + port + "/")
	latencyMS = time.Since(start).Milliseconds()
	if err == nil {
		resp.Body.Close()
		return "responding", "", latencyMS
	}

	msg := err.Error()
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "not_responding", "local app accepted the request but did not send HTTP response headers", latencyMS
	case strings.Contains(msg, "connection refused"):
		return "not_listening", "nothing is listening on localhost:" + port, latencyMS
	case strings.Contains(msg, "malformed HTTP response"):
		return "not_http", "localhost:" + port + " is listening, but it did not speak HTTP", latencyMS
	default:
		return "error", msg, latencyMS
	}
}

func monitorLocalHTTP(port string, status *Status) {
	for {
		state, msg, latency := probeLocalHTTP(port, 3*time.Second)
		status.setLocal(state, msg, latency, port)
		time.Sleep(10 * time.Second)
	}
}

func serveStatus(tunnelPort string, status *Status) {
	sp := statusPort(tunnelPort)
	addr := fmt.Sprintf("localhost:%d", sp)

	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(status.snapshot())
	})

	// Probes the tunnel end-to-end using the health-check shortcut so no local
	// server is needed. Returns latency and ok/fail for the dashboard history.
	mux.HandleFunc("/api/probe", func(w http.ResponseWriter, r *http.Request) {
		snap := status.snapshot()
		surl, _ := snap["url"].(string)
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
    <div class="stat"><strong id="local">—</strong>local app</div>
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
    const localMap={responding:'OK',not_listening:'down',not_responding:'stuck',not_http:'not HTTP',unknown:'—',error:'error'};
    const local=document.getElementById('local');
    local.textContent=localMap[d.local_state]||d.local_state||'—';
    local.title=d.local_error||'';
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

	if os.Args[1] == "--tcp" {
		fmt.Fprintln(os.Stderr, "  error: TCP tunnels are moving to a separate relay and are not available in this build")
		os.Exit(1)
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

	localState, localMsg, latency := probeLocalHTTP(port, 1500*time.Millisecond)
	status.setLocal(localState, localMsg, latency, port)
	switch localState {
	case "responding":
	case "not_listening":
		fmt.Fprintf(os.Stderr, "  warning: nothing is listening on localhost:%s — the tunnel will stay online and recover when your app starts\n\n", port)
	case "not_responding":
		fmt.Fprintf(os.Stderr, "  warning: localhost:%s accepts connections but did not return HTTP headers — public requests may return 504 until it responds\n\n", port)
	case "not_http":
		fmt.Fprintf(os.Stderr, "  warning: localhost:%s is listening but does not appear to speak HTTP\n\n", port)
	default:
		fmt.Fprintf(os.Stderr, "  warning: local app check failed: %s\n\n", localMsg)
	}
	go monitorLocalHTTP(port, status)

	// Check for updates in the background — prints notice after tunnel is live.
	versionNoticeCh := make(chan string, 1)
	go func() {
		latest, required := checkCLIVersion()
		if latest == "" || !versionLessThan(version, latest) {
			versionNoticeCh <- ""
			return
		}
		if versionLessThan(version, required) {
			fmt.Fprintf(os.Stderr, "\n  sidedoor v%s is required — run: npm update -g @sidedoor/cli\n\n", latest)
			os.Exit(1)
		}
		versionNoticeCh <- fmt.Sprintf("  ⚠  sidedoor v%s available — run: npm update -g @sidedoor/cli\n", latest)
	}()

	fmt.Print("\n  sidedoor connecting...\n\n")
	pinnedMachine := ""
	consecutiveFailures := 0
	noticePrinted := false
	for {
		status.setState("connecting", "", port)
		nextMachine, err := tryConnect(relayURL, port, token, pinnedMachine, status)

		if !noticePrinted {
			select {
			case notice := <-versionNoticeCh:
				if notice != "" {
					fmt.Print(notice)
				}
				noticePrinted = true
			default:
			}
		}

		if nextMachine == "replaced" {
			fmt.Fprintf(os.Stderr, "\n  another sidedoor session started for this account — stopping\n\n")
			os.Exit(0)
		}

		if err != nil {
			if errors.Is(err, errFatal) {
				os.Exit(1)
			}
			consecutiveFailures++
		} else {
			consecutiveFailures = 0
		}

		if nextMachine == "reset" || consecutiveFailures >= 5 {
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

	handshake := "token:" + token + " port:" + port
	if _, err := netConn.Write([]byte(handshake + "\n")); err != nil {
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
		return "", errFatal
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
