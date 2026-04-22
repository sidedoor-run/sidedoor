# Scaling

## Current capacity

### Memory cost per tunnel (relay)

| State | RAM per tunnel |
|---|---|
| Idle (connected, no traffic) | ~40–60KB |
| Active (proxying HTTP requests) | ~200KB–2MB depending on concurrency |

Each tunnel creates: 3–4 goroutines (~30KB), yamux session state (~8KB), WebSocket buffers (~8KB). The `MaxStreamWindowSize = 16MB` is a maximum per stream — not allocated upfront. Actual usage tracks real traffic.

### Per-machine limits (512MB shared-cpu-1x)

| Workload | Tunnels per machine | Notes |
|---|---|---|
| All idle | ~7,000 | RAM-bound |
| Mixed (typical dev use) | ~2,000–4,000 | CPU starts mattering |
| Heavy traffic (file uploads, streaming) | ~200–500 | yamux windows + copy buffers stack up |

### Current fleet (3 machines)

| Workload | Total capacity |
|---|---|
| Idle tunnels | ~20,000 |
| Active (typical) | ~6,000–12,000 |
| Fly.io hard connection limit | 15,000 (5,000 × 3) |

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

### Stage 1 — 0 to 500 concurrent users
**Current setup handles this. No changes needed.**

Cost: ~$15/month (3 × shared-cpu-1x 512MB machines).

Action items:
- Fix goroutine leak in `openStream`
- Add Redis replica for availability

### Stage 2 — 500 to 3,000 concurrent users
**Upgrade machine size. Add regions.**

```bash
# Upgrade all machines to 2GB RAM
fly machine update e827942c0050d8 --vm-memory 2048 --app sidedoor-relay
fly machine update 6e826329b6e087 --vm-memory 2048 --app sidedoor-relay
fly machine update d8d2474b122958 --vm-memory 2048 --app sidedoor-relay

# Add machines in high-demand regions
fly machine clone e827942c0050d8 --region sin   # Singapore
fly machine clone e827942c0050d8 --region syd   # Sydney
fly machine clone e827942c0050d8 --region lhr   # London
```

Add rate limiting per subdomain.

Cost: ~$80–120/month.

### Stage 3 — 3,000 to 20,000 concurrent users
**Dedicated CPUs. Redis cluster. More regions.**

```bash
# Switch to dedicated CPU
fly machine update <id> --vm-cpu-kind performance --vm-cpus 2 --vm-memory 4096
```

- Move from single Redis instance to Redis Cluster or Upstash with replication + failover
- Implement `/api/me` on auth server (returns `{subdomain, tier, max_tunnels}`) so limits are server-enforced, not client-side
- Add admin kill endpoint for abuse control
- Add per-subdomain rate limiting if not already done

Cost: ~$300–600/month.

### Stage 4 — 20,000+ concurrent users
**Architectural work required.**

- Evaluate replacing Redis pub/sub with a purpose-built service mesh for session routing
- Consider multi-region Redis with consistent hashing
- Add observability (metrics per tunnel, per machine, per region)
- Consider dedicated machines vs shared Fly.io infrastructure for predictable latency
- TCP tunnel support (each user gets a dedicated port — requires port pool management in Redis and Fly.io port range reservation)

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
