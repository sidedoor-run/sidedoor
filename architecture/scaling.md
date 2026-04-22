# Scaling

## Current capacity

### Memory cost per tunnel (relay)

| State | RAM per tunnel |
|---|---|
| Idle (connected, no traffic) | ~40–60KB |
| Active (proxying HTTP requests) | ~200KB–2MB depending on concurrency |

Each tunnel creates: 3–4 goroutines (~30KB), yamux session state (~8KB), WebSocket buffers (~8KB). The `MaxStreamWindowSize = 16MB` is a maximum per stream — not allocated upfront. Actual usage tracks real traffic.

### Per-machine limits (512MB shared-cpu-1x) — realistic numbers

The memory math allows far more tunnels than the CPU can actually serve. `shared-cpu-1x` means burst CPU shared with other Fly.io tenants on the same physical host — noisy neighbours directly degrade your users' latency. CPU saturates well before RAM runs out.

| Workload | Comfortable (no degradation) | Degraded but working |
|---|---|---|
| Idle tunnels | ~300–500 | ~500–2,000 |
| Mixed (typical dev use) | ~100–300 | ~300–800 |
| Heavy traffic (file uploads, streaming) | ~20–50 | ~50–200 |

### Current fleet (3 machines)

| Workload | Comfortable | Degraded but working |
|---|---|---|
| Concurrent tunnels | 300–900 | 900–2,400 |
| Fly.io hard connection limit | 15,000 (5,000 × 3) | — |

The Fly.io connection limit is not the constraint. CPU is. The goroutine leak in `openStream` makes this worse over time — leaked goroutines accumulate under load and slowly eat both CPU and memory until the process restarts.

---

## Bottlenecks in order

**1. CPU before memory at real load**
`shared-cpu-1x` is burst CPU — shared with other Fly.io tenants. Under sustained proxying (10+ concurrent requests per tunnel) you'll see latency spikes. This shows up before RAM runs out.

**2. Redis availability**
Redis is a single instance with no replica. If it goes down, cross-region forwarding breaks entirely. Agents already connected to the right machine keep working; requests landing on the wrong machine get 502s until the agent reconnects. Fix: add a read replica or switch to Upstash Redis with replication.

**3. Goroutine leak on stream open timeout**
`relay/main.go openStream()` has a goroutine that leaks on timeout — the spawned goroutine stays blocked on `session.Open()` until yamux errors the session. Under sustained timeout conditions this accumulates. Fix: pass a `context.WithTimeout` to cancel the goroutine.

**4. No rate limiting per subdomain**
A single tunnel can receive unlimited inbound traffic with no throttle. One abusive tunnel can saturate a machine's network. Fix: middleware in the relay HTTP handler, keyed by subdomain.

---

## Scaling path

### Stage 1 — 0 to 300 concurrent users
**Current setup handles this with acceptable quality.**

Cost: $9.57/month (3 × shared-cpu-1x 512MB @ $3.19 each).

Action items before launching publicly:
- Fix goroutine leak in `openStream` — gets worse exactly when you need reliability
- Add Redis replica for availability

### Stage 2 — 300 to 2,000 concurrent users
**Upgrade to dedicated CPU. This is the most important single change.**

Shared CPU is the binding constraint — not RAM, not connections. Switching to `performance-1x` eliminates noisy-neighbour degradation and gives consistent latency across all users.

```bash
fly machine update e827942c0050d8 --vm-cpu-kind performance --vm-cpus 1 --vm-memory 2048 --app sidedoor-relay
fly machine update 6e826329b6e087 --vm-cpu-kind performance --vm-cpus 1 --vm-memory 2048 --app sidedoor-relay
fly machine update d8d2474b122958 --vm-cpu-kind performance --vm-cpus 1 --vm-memory 2048 --app sidedoor-relay
```

Add rate limiting per subdomain to prevent one user saturating a machine's network.

Cost: ~$93/month (3 × performance-1x 2GB @ $31 each).

### Stage 3 — 2,000 to 10,000 concurrent users
**More machines. More regions. Redis HA.**

```bash
# Add machines in high-demand regions
fly machine clone e827942c0050d8 --region sin   # Singapore
fly machine clone e827942c0050d8 --region syd   # Sydney
fly machine clone e827942c0050d8 --region lhr   # London
```

- Move Redis to Upstash with replication + failover (removes the single point of failure)
- Implement `/api/me` on auth server — returns `{subdomain, tier, max_tunnels}` so limits are server-enforced
- Add admin kill endpoint for abuse control

Cost: ~$250–400/month (6–8 performance-1x machines + Redis HA).

### Stage 4 — 10,000+ concurrent users
**Architectural work required.**

- Multi-region Redis with consistent hashing or purpose-built session routing
- Observability: metrics per tunnel, per machine, per region
- TCP tunnel support (port pool management in Redis + Fly.io port range reservation)
- Evaluate moving off shared Fly.io infrastructure to dedicated hardware for largest regions

Cost: $1,000+/month, depends heavily on traffic patterns.

---

## Fly.io scaling commands reference

```bash
# Check machine status
fly machine list --app sidedoor-relay

# Scale machines horizontally
fly machine clone <id> --region <region> --app sidedoor-relay

# Upgrade machine size
fly machine update <id> --vm-memory 2048 --vm-cpu-kind shared --app sidedoor-relay

# Stop / start a machine (testing)
fly machine stop <id> --app sidedoor-relay
fly machine start <id> --app sidedoor-relay

# Deploy new relay version
cd /Users/jonathan/Projects/sidedoor
fly deploy

# View logs
fly logs --app sidedoor-relay
```

---

## What playit.gg has that we don't (and when it matters)

| Capability | playit.gg | sidedoor | When it matters |
|---|---|---|---|
| UDP tunnels | ✓ | ✗ | Game servers, VoIP |
| TCP tunnels (non-HTTP) | ✓ | ✗ (planned) | SSH, databases |
| 19 dedicated datacenters | ✓ | 3 Fly.io machines | Sub-50ms latency globally |
| DDoS protection (gaming) | ✓ | ✗ | Minecraft-scale attacks |
| MCP / AI integration | ✗ | ✓ | AI-assisted dev workflows |
| npm distribution | ✗ | ✓ | Developer toolchains |
| Programmatic tunnel management | ✗ | ✓ | CI/CD, automation |

For developer use cases (HTTP, APIs, dev servers): Fly.io infrastructure is more than sufficient. The latency difference is invisible at 100ms+ round trips typical of dev workflows.
