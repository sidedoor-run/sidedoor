# Current Infrastructure

## Fly.io app: `sidedoor-relay`

### Machines

| ID | Name | Region | Size |
|---|---|---|---|
| e827942c0050d8 | winter-bird-9403 | GRU (São Paulo) | shared-cpu-1x / 512MB |
| 6e826329b6e087 | dry-firefly-57 | IAD (Virginia) | shared-cpu-1x / 512MB |
| d8d2474b122958 | small-waterfall-8734 | FRA (Frankfurt) | shared-cpu-1x / 512MB |

Primary region: FRA. Machines run continuously (`auto_stop_machines = false`).

### Fly.io concurrency config

```toml
[http_service.concurrency]
  type = "connections"
  hard_limit = 5000
  soft_limit = 4000
```

Fly's proxy starts routing new connections to less-loaded machines at soft_limit. At hard_limit it rejects connections. These are per-machine limits — 15,000 total across the fleet.

### Relay image

Built from `relay/Dockerfile` on every push to a `v*` tag. Deployed with `fly deploy` from the repo root (fly.toml is at root, not in relay/).

```
cd /Users/jonathan/Projects/sidedoor
fly deploy
```

### Environment variables (per machine)

| Variable | Purpose |
|---|---|
| `REDIS_URL` | Redis connection string for cross-region routing |
| `AUTH_URL` | Auth server base URL (`sidedoor-eight.vercel.app`) |
| `COLOR_DOMAINS` | Comma-separated tunnel domains (`sidedoor.pink,sidedoor.red,...`) |
| `FLY_MACHINE_ID` | Set by Fly.io — used to build internal machine address |
| `FLY_APP_NAME` | Set by Fly.io |
| `FLY_REGION` | Set by Fly.io — logged on agent connect |

---

## Redis

Used for cross-region session routing. One entry per active tunnel:

```
tunnel:<subdomain>  →  http://<fly-internal-addr>:8080
TTL: 90s, refreshed every 30s by relay keepalive goroutine
```

**Single instance — no replica or failover.** If Redis goes down, cross-region forwarding breaks. Agents pinned to their connected machine still work; only requests landing on the wrong machine fail.

---

## Auth server: `sidedoor-eight.vercel.app`

Vercel serverless deployment. Handles:
- `POST /api/oauth/device/code` — issues device code + user code
- `POST /api/oauth/device/token` — polls for token, returns JWT on approval
- `GET /api/tunnel/subdomain` — returns `{subdomain, domain, max_tunnels}` for authenticated token

Subdomains are assigned per account token. Each port gets a unique URL: `<base>-<port>.<domain>` (e.g. `papita-3000.sidedoor.pink`). `max_tunnels` controls how many concurrent tunnels that account may open; the relay defaults to 1 if the field is absent or zero.

---

## npm registry: `@sidedoor/cli`

Published to npmjs.com. The wrapper (`npm/index.js`) downloads the Go binary from GitHub releases on first run. Binary is cached at `~/.sidedoor/bin/sidedoor`. Re-downloaded only when the npm package version changes.

### Release process

Push a `v*` tag to `sidedoor-run/sidedoor`. GitHub Actions:
1. Builds Go binaries for darwin/linux/windows × amd64/arm64 on macOS and Ubuntu runners
2. Ad-hoc codesigns darwin binaries (`codesign --sign -`)
3. Creates GitHub release with all binaries as assets
4. Bumps npm package version to match tag
5. Publishes `@sidedoor/cli` to npmjs.com

Requires `NPM_TOKEN` secret in GitHub repo settings.

---

## Domains

| Domain | Purpose |
|---|---|
| `sidedoor.run` | Relay WebSocket endpoint (`wss://sidedoor.run/sidedoor/connect`) |
| `*.sidedoor.pink` | Tunnel subdomains (e.g. `papita.sidedoor.pink`) |
| `sidedoor-eight.vercel.app` | Auth server |

Additional color domains configured in `COLOR_DOMAINS` env var on relay machines.

---

## Repositories

| Repo | Visibility | Contents |
|---|---|---|
| sidedoor-run/sidedoor | Public | Agent (Go), npm wrapper, GitHub Actions release workflow |
| sidedoor-run/relay | Private | Relay server (Go), Dockerfile |
| sidedoor-run/mcp | Private | MCP server (TypeScript) |
| sidedoor-run/desktop | Private | macOS menu bar app (Swift) |
