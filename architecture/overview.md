# System Overview

## What sidedoor is

A localhost tunnel service. A user runs `sidedoor 3000` on their machine and gets a public HTTPS URL that proxies to their local port. The connection is persistent, auto-reconnects on drop, and routes through a relay running on Fly.io.

---

## Components

| Component | Language | Repo | Role |
|---|---|---|---|
| `relay` | Go | sidedoor-run/relay | Private server on Fly.io — accepts agent WebSocket connections, proxies inbound HTTP |
| `agent` | Go | sidedoor-run/sidedoor (`agent/`) | CLI binary — connects to relay, proxies streams to localhost |
| `npm` | Node.js | sidedoor-run/sidedoor (`npm/`) | Thin wrapper — downloads versioned Go binary on first run, handles signals |
| `mcp` | TypeScript | sidedoor-run/mcp | MCP server — lets AI assistants (Claude, Cursor) open and manage tunnels |
| `desktop` | Swift | sidedoor-run/desktop | macOS menu bar app — shows all running tunnels and their live status |
| `auth` | Vercel | sidedoor-eight.vercel.app | Auth server — OAuth device flow, issues JWTs, assigns subdomains |

---

## Traffic flow

```
Browser / API client
    │
    ▼ HTTPS
Fly.io edge (anycast — routes to nearest machine)
    │
    ▼
Relay process (GRU / IAD / FRA)
    │
    ├── subdomain in local registry? ──► yamux stream ──► agent ──► localhost:port
    │
    └── not found ──► Redis lookup ──► HTTP forward to correct machine
                                              │
                                              └── session not found ──► delete stale Redis entry, return 502
                                                                              │
                                                                              ▼
                                                                    Agent health check detects 502 ──► reconnect
```

---

## Connection protocol

```
Agent → relay:  token:<jwt>\n
Relay → agent:  ok:https://<sub>.<domain>|machine:<fly-machine-id>\n   (success)
Relay → agent:  error:<message>\n                                        (failure)
```

Both sides then wrap the WebSocket in yamux. The relay opens one yamux stream per inbound HTTP request. The agent accepts streams and proxies each to `localhost:<port>`.

---

## Agent state machine

```
connecting ──► running ──► reconnecting ──► connecting
                │
                └── (health check fails or ping timeout) ──► reconnecting
```

State is written to `~/.sidedoor/agent-<port>.json` on every transition so the desktop app and status dashboard always reflect the real state.

---

## Local status server

Each running agent exposes `http://localhost:{port+10000}`:

- `GET /` — HTML dashboard (live dot, URL, uptime, reconnect count, probe history)
- `GET /api/status` — JSON state for programmatic use
- `GET /api/probe` — fires a health-check request through the tunnel, returns latency

```
sidedoor 3000  →  status at http://localhost:13000
sidedoor 8080  →  status at http://localhost:18080
```

---

## Multi-region routing

Each relay machine registers `tunnel:<subdomain> → <fly-internal-addr>` in Redis with a 90s TTL. A keepalive goroutine refreshes it every 30s. On a miss the relay does an HTTP forward to the owning machine using `X-Sidedoor-Internal` header to prevent forwarding loops. If the owning machine no longer has the session it deletes the stale Redis entry and returns 502 — the agent's health check catches this and reconnects.

---

## Health check

The agent hits its own public URL with `X-Sidedoor-Health: 1` every 30s. The relay intercepts this header and returns 200 immediately without forwarding to the local server. If the agent gets anything other than 200 it knows routing is broken, clears its machine pin, and reconnects to any available machine.

---

## File locations (per user machine)

| File | Written by | Purpose |
|---|---|---|
| `~/.sidedoor/token` | Agent (`sidedoor auth`) | JWT — 0600 permissions |
| `~/.sidedoor/mcp-state.json` | MCP server + desktop app | Active tunnel registry for external consumers |
| `~/.sidedoor/agent-<port>.json` | Agent | Live connection state for desktop app |
| `~/.sidedoor/bin/sidedoor` | npm wrapper | Versioned Go binary |
