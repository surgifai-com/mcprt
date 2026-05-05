# mcprt

**On-demand process supervisor for Streamable-HTTP MCP servers.**  
Servers spawn when a client connects. They stop when the last client disconnects.  
No timeouts. No always-on resident processes. STDIO transport refused.

**License**: Apache 2.0 — Copyright 2026 Surgifai / Victor Q. Nguyen

---

This is why it was built:

```
panic(cpu 1 caller 0xfffffe0034ce0f2c): watchdog timeout: no checkins from
watchdogd in 93 seconds
Compressor Info: 100% of segments limit (BAD) with 45 swapfiles
```

A 16 GB Apple Silicon Mac Mini running five always-on MCP servers hit VM compressor saturation and hard-rebooted — twice. Stopping the MCP services reclaimed ~500 MB of idle RAM and eliminated the panics. The open question was how to get that memory back without losing the servers when actually needed. This is the answer.

---

## Is this for you?

| Your situation | mcprt |
|---|---|
| Running 3+ local MCP servers (Claude Code, Cline, Continue) | Yes — this is the core use case |
| On a memory-constrained machine (8–16 GB) | Yes — idle footprint drops to ~30 MB total |
| MCP servers that crash mid-session and you want auto-restart | Yes — supervisor handles it |
| Building a tool that embeds MCP lifecycle management | Yes — library-first design |
| Your MCP servers use STDIO transport | No — mcprt refuses STDIO unconditionally (see [why](#problem-3-stdio-transport-is-a-security-liability-hiding-in-plain-sight)) |
| Need multi-host or cloud MCP orchestration | No — single host only, by design |

---

## How it compares

| Approach | Idle RAM | Cold-start | STDIO | Infrastructure |
|---|---|---|---|---|
| Always-on launchd/systemd services | ~100–300 MB per server | 0 ms | Yes | None |
| Idle-timeout eviction (mcp-hub, scripts) | Partial savings | 0 ms if warm | Yes | None |
| **mcprt** | **~30 MB total** | **~500 ms** | **No — refused** | **None** |
| microsoft/mcp-gateway | High | ~0 ms | Yes (wrapper) | Kubernetes + Redis + Azure |

The tradeoff mcprt makes explicitly: you pay ~500 ms cold-start latency on the first connection after the server has been idle. In exchange you get near-zero idle memory and a hard STDIO prohibition. If you need sub-100ms cold starts or STDIO support, mcprt is not your tool.

---

## Problems

### Problem 1: MCP server processes stay resident forever, burning memory around the clock

Every server you add to `~/.claude/mcp.json` (or your Cline/Continue config) spawns a process that runs 24/7 — whether you're using it or not:

| Server | Idle RSS |
|---|---|
| vault-mcp (bge-m3 embeddings loaded) | ~280 MB |
| google-analytics-mcp | ~110 MB |
| google-ads-mcp | ~115 MB |
| chrome-devtools-mcp | ~60 MB |
| **Total, 4 servers** | **~565 MB always occupied** |

On 8 GB machines this directly causes swap. On Apple Silicon it competes with the unified memory pool that GPU and Neural Engine also share. At saturation, macOS watchdogd triggers a kernel panic and the machine reboots.

### Problem 2: Idle-timeout eviction is the wrong heuristic

The naive fix — "kill the server if no request in N minutes" — fails in both directions:

- **Too aggressive**: a server mid-task that paused for 3 minutes gets killed. The next tool call fails mid-context. The user blames the AI.
- **Too lax**: a 30-minute timeout barely saves memory during typical work sessions.
- **Fundamentally wrong signal**: "inactivity" and "no open connections" are different things. A server can be silent for 10 minutes because the model is reasoning, not because the session ended. Only connection close is the reliable termination signal.

mcp-hub and most homebuilt shell wrappers implement idle-timeout. mcprt explicitly does not.

### Problem 3: STDIO transport is a security liability hiding in plain sight

In April 2026, OX Security disclosed 14 CVEs across the MCP ecosystem — LiteLLM, LangChain, LangFlow, Flowise, LettaAI, LangBot — affecting 200K+ server deployments and 150M+ package downloads. Root cause: STDIO transport runtimes treat `mcp.json` as an exec list. A malicious or compromised package entry runs with your full user context, filesystem access, and credential environment. Anthropic acknowledged this is intentional behavior and has no plans to patch it.

The STDIO process model used by Claude Desktop, Anthropic's `mcp-builder` skill, and most MCP documentation is load-bearing attack surface. mcprt's policy validator catches this at config load — before any process runs.

### Problem 4: The only alternative that exists is enterprise infrastructure for a single-developer problem

[microsoft/mcp-gateway](https://github.com/microsoft/mcp-gateway) is competently built. It is also Kubernetes StatefulSets, Redis, Azure Entra ID, a STDIO-wrapper proxy, and a dedicated ops team. The engineering cost of running mcp-gateway for a local developer setup exceeds the cost of the problem it's solving.

There was no lightweight, opinionated, single-binary tool that said "MCP servers should run iff a client is talking to them." mcprt is that tool.

---

## How mcprt solves it

### Connection-refcounted lifecycle

mcprt runs a single proxy on `127.0.0.1:9090`. Each MCP server gets a named route (`/vault-mcp/...`, `/ga-surgifai/...`). The upstream process only exists while at least one client connection is open.

```
Before (always-on launchd):   4 MCP servers = ~565 MB idle, always
After  (mcprt):               mcprt daemon only = ~30 MB idle
                              servers spawn on first connection, ~500ms cold start
                              servers stop ~5s after last client disconnects
```

### Refcount, not timeout

mcprt reads two signals from the MCP Streamable HTTP transport:

- **Primary**: open SSE streams (`Accept: text/event-stream`). One open stream = `refcount++` for that server.
- **Secondary**: `Mcp-Session-Id` headers on POST requests without an SSE stream. Tracked as ephemeral sessions — the ref is held until the TCP connection closes, not until a timer fires.

Lifecycle transitions:
- `refcount 0 → 1`: proxy holds the request, spawns upstream, waits for health check (default max 5s), then forwards.
- `refcount stable at 0 for grace_period (default 5s)`: SIGTERM → SIGKILL after grace window.

A server mid-task has at least one open SSE stream. Refcount is ≥ 1. It cannot be stopped by this logic no matter how long the model takes to respond. This is the categorical difference from idle-timeout.

### STDIO refused at config load — before any process runs

```
$ mcprt validate ~/.config/mcprt/mcprt.toml

ERROR   [error] server "bad-server": exec uses "npx" — a process launcher that
        commonly wraps STDIO MCPs. If this binary genuinely starts an HTTP MCP
        server, set acknowledged_stdio_safe=true.
ERROR   [error] server "bad-server": arg "@modelcontextprotocol/server-filesystem"
        matches known STDIO MCP package pattern "@modelcontextprotocol/server-".
ERROR   [error] server "bad-server": server args do not contain ${MCPRT_PORT} or
        a --port/--bind flag; mcprt cannot determine where to proxy requests

~/.config/mcprt/mcprt.toml: 2 error(s), 0 warning(s)
exit code 1
```

The validator runs at startup and on every hot-reload. Refused entries cannot slip through between reloads. `mcprt validate` exits non-zero — wire it into CI.

Detected patterns: `npx`, `node`, `python`, `python3`, `deno`, `bun` as exec binary; `@modelcontextprotocol/server-*`, `mcp-server-*`, `-mcp-server` as package name fragments; missing `--port`/`${MCPRT_PORT}` argument.

To acknowledge a Python binary that genuinely binds HTTP: set `acknowledged_stdio_safe = true` — you get a warning logged at every load as an audit trail.

---

## Architecture

```
Claude Code / Cline / Continue
        │  HTTP  (Streamable HTTP transport)
        ▼
mcprt  127.0.0.1:9090
        │
        ├─ /vault-mcp/...    RefCounter ── Supervisor ── vault-mcp  :19000  (running)
        ├─ /ga-surgifai/...  RefCounter ── Supervisor ── analytics  :19001  (idle)
        └─ /google-ads/...   RefCounter ── Supervisor ── ads-mcp    :19002  (idle)
```

- **Proxy** (`pkg/proxy`): routes on first path segment, strips it, reverse-proxies to upstream.
- **RefCounter** (`pkg/proxy`): tracks SSE streams + ephemeral sessions; debounces zero-transition.
- **Supervisor** (`pkg/supervisor`): `idle → spawning → running → stopping`; health-check poll; SIGTERM + SIGKILL.
- **Runtime** (`pkg/runtime`): dynamic port allocation; fsnotify hot-reload; reconciliation diff on config change.

---

## Quickstart

```sh
# Install
go install github.com/victorqnguyen/mcprt/cmd/mcprt@latest

# Config
mkdir -p ~/.config/mcprt
cat > ~/.config/mcprt/mcprt.toml << 'EOF'
[runtime]
listen       = "127.0.0.1:9090"
grace_period = "5s"

[server.vault-mcp]
exec        = ["/path/to/vault-mcp/.venv/bin/vault-mcp"]
args        = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
health_path = "/health"

[server.ga-mysite]
exec = ["/path/to/analytics-mcp/.venv/bin/analytics-mcp"]
args = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
env  = { GOOGLE_APPLICATION_CREDENTIALS = "~/.config/mcprt/secrets/mysite.json" }
EOF

# Validate (catches STDIO violations before anything runs)
mcprt validate ~/.config/mcprt/mcprt.toml

# Start
mcprt serve
```

**Point Claude Code at mcprt** (`~/.claude/mcp.json`):

```json
{
  "mcpServers": {
    "vault-mcp":  { "type": "http", "url": "http://localhost:9090/vault-mcp/mcp" },
    "ga-mysite":  { "type": "http", "url": "http://localhost:9090/ga-mysite/mcp" }
  }
}
```

**macOS service** (starts at login):

```sh
# Edit dist/launchd/com.mcprt.daemon.plist — substitute YOUR_USERNAME
cp dist/launchd/com.mcprt.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.mcprt.daemon.plist
```

---

## Manifest reference

```toml
[runtime]
listen       = "127.0.0.1:9090"   # proxy bind address
log_level    = "info"              # debug | info | warn | error
grace_period = "5s"                # debounce before stopping (not an idle timeout)

[server.<name>]
exec                    = ["/abs/path/to/binary"]
args                    = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
env                     = { KEY = "value" }
health_path             = "/health"    # HTTP GET probe; omit for 500ms fixed delay
working_dir             = "/path/to"
allow_external          = false        # non-loopback bind; default false
acknowledged_stdio_safe = false        # suppress launcher warning; leave a comment why
```

`${MCPRT_PORT}` is the only variable. mcprt allocates one port per server from 19000 upward and substitutes it in both `args` and `env`.

---

## CLI

| Command | What it does |
|---|---|
| `mcprt serve` | Start the proxy daemon |
| `mcprt validate <file>` | Policy check; exits 1 on errors |
| `mcprt validate --json <file>` | Same, machine-readable |
| `mcprt status` | State, PID, port, restart count for all servers |
| `mcprt status --json` | Same, as JSON |

Hot-reload is automatic — edit `mcprt.toml` while `serve` is running. Policy re-validates on every reload; invalid entries are refused without disrupting running servers.

---

## Embedding the library

The CLI is one consumer of the library. Import it directly:

```go
import (
    "github.com/victorqnguyen/mcprt/pkg/manifest"
    "github.com/victorqnguyen/mcprt/pkg/policy"
    "github.com/victorqnguyen/mcprt/pkg/runtime"
)

// Lint a manifest without starting anything
cfg, _ := manifest.Load("mcprt.toml")
violations := policy.Validate(cfg)

// Full runtime with state hooks
rt, _ := runtime.New(runtime.Options{ManifestPath: "mcprt.toml"})
rt.OnSpawn = func(name string) { /* dashboard: mark running */ }
rt.OnExit  = func(name string) { /* dashboard: mark idle */ }
rt.Serve(ctx)
```

Public API: `runtime.Runtime`, `manifest.Config`, `manifest.ServerSpec`, `proxy.Handler`, `proxy.RefCounter`, `supervisor.Supervisor`, `supervisor.Stats`, `policy.Validate`, `policy.Violation`.

---

## Security model

- Upstream servers bind `127.0.0.1` by default. `allow_external = true` required for any other interface.
- STDIO refused unconditionally. Not a flag. Not a default. A hard validator error.
- Policy runs at startup and on every hot-reload — no window between reloads where a bad entry can run.
- mcprt does not log, store, or inspect credential values. Env vars pass to child processes as-is.
- Path prefix is stripped before forwarding — upstream servers see only their own routes.

---

## Roadmap

- [ ] `mcprt status --watch` — live TUI (bubbletea)
- [ ] `mcprt logs <server>` — tail per-server stdout/stderr
- [ ] `mcprt up <server>` / `mcprt down <server>` — manual overrides without editing config
- [ ] Prometheus `/metrics` endpoint (opt-in, off by default)
- [ ] Homebrew formula
- [ ] systemd unit (`dist/systemd/`)
- [ ] RSS + CPU sampling in `mcprt status` (gopsutil already a dep)

**Explicitly out of scope for v1:** multi-host clustering, web/GUI dashboard, idle-timeout fallback mode, STDIO support of any kind. These are not future features. They are non-goals.

---

## Contributing

Apache 2.0. PRs welcome.

Rules:
- Policy validator additions require a CVE reference or public disclosure link in the commit message.
- Transport additions must use Streamable HTTP. STDIO PRs will be closed without discussion.

```
mcprt/
├── pkg/
│   ├── manifest/   TOML loader, schema, ~ and ${MCPRT_PORT} expansion
│   ├── policy/     STDIO detector, port-binding check, violation types
│   ├── proxy/      Streamable HTTP reverse proxy, RefCounter
│   ├── supervisor/ Process lifecycle state machine
│   └── runtime/    Orchestrator, port allocator, fsnotify hot-reload
├── cmd/mcprt/      CLI (serve, validate, status)
├── dist/           launchd plist template (systemd + Homebrew coming)
└── examples/       mcprt.toml + mcp.json for Claude Code
```
