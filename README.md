# mcprt

**MCP runtime for developers who care about memory, security, and not getting paged at 3am.**

`mcprt` is a single Go binary that manages your local MCP servers on demand. Servers start when a client connects and stop when the last client disconnects — no timeouts, no always-on processes, no idle RAM tax.

**License**: Apache 2.0 — Copyright 2026 Surgifai / Victor Q. Nguyen

---

## Problems

### Problem 1: Your MCP servers are always on, burning hundreds of megabytes around the clock

Every MCP server you add to `~/.claude/mcp.json` (or your Cline/Continue config) spawns a process that stays resident forever — whether you're using it or not. A typical local stack:

| Server | Idle RSS |
|---|---|
| vault-mcp (bge-m3 embeddings loaded) | ~280 MB |
| google-analytics-mcp | ~110 MB |
| google-ads-mcp | ~115 MB |
| chrome-devtools-mcp | ~60 MB |
| **Total** | **~565 MB** |

That's over half a gigabyte reserved for tools you might not touch for hours. On a 16 GB machine this crowds out your actual work. On an 8 GB machine it causes swap. On Apple Silicon it can push the VM compressor into saturation — which on macOS triggers a watchdog panic and a hard reboot. This is not hypothetical; it's what prompted mcprt's creation.

### Problem 2: Idle-timeout eviction is the wrong heuristic

The obvious fix — "kill the server if no request in N minutes" — has a fundamental flaw: it asks the wrong question.

- **Too aggressive**: a server mid-task that hasn't received a request in 3 minutes gets killed. The next tool call fails with a connection error. The user blames the AI.
- **Too lax**: set 30-minute timeouts and you negate the memory savings.
- **No right answer**: the timeout is tuning a heuristic that's wrong in both directions. The right signal isn't "inactivity" — it's "no open connections."

Idle-timeout eviction exists in mcp-hub, various shell scripts, and homebuilt wrappers. mcprt explicitly does not implement it.

### Problem 3: STDIO transport is a security liability hiding in plain sight

In April 2026, OX Security disclosed 14 CVEs across 200K+ MCP servers totaling 150M+ downloads — LiteLLM, LangChain, LangFlow, Flowise, LettaAI, LangBot. The root cause: MCP runtimes that use STDIO transport, treating your `mcp.json` as an `exec` list. Any manifest entry pointing at a malicious package gets run with your full user context, credential access, and filesystem permissions. Anthropic classified the design as intentional and has no patch plans.

This means the STDIO runtimes shipped by default — including Anthropic's own `mcp-builder` skill and Claude Desktop's process model — are load-bearing attack surface for supply-chain compromises.

### Problem 4: The heavyweight alternatives require infrastructure you don't have

[microsoft/mcp-gateway](https://github.com/microsoft/mcp-gateway) is a real system with real engineering behind it. It is also Kubernetes StatefulSets, Redis, Azure Entra ID, and a STDIO-wrapper proxy. Wrong scale for a laptop. Wrong security model for a single developer. Wrong everything.

---

## How mcprt solves it

### Solution to Problem 1: Connection-refcounted lifecycle

mcprt proxies all your MCP servers through a single local port (`127.0.0.1:9090`). Each server gets a named route (`/vault-mcp/...`, `/ga-surgifai/...`). The server process only exists while a client is connected.

```
Before (always-on):   vault-mcp + 3× analytics-mcp + ads-mcp = ~565 MB idle
After (mcprt):        mcprt daemon only                        = ~30 MB idle
```

When Claude Code opens a session, `vault-mcp` spawns in ~500ms. When the session ends, `vault-mcp` gets SIGTERM. The debounce window (default 5s) absorbs rapid reconnects so you don't thrash on reconnect.

### Solution to Problem 2: Refcount, not timeout

mcprt tracks two connection signals from the MCP Streamable HTTP transport:

- **Primary**: persistent SSE streams (`Accept: text/event-stream`). One open SSE = +1 to the server's refcount.
- **Secondary**: `Mcp-Session-Id` headers on POST requests with no accompanying SSE. These are tracked as ephemeral sessions tied to the TCP connection's lifetime — not a wall-clock timer.

When refcount drops to zero **and stays there** for the grace period, the server stops. "Stays there" is the key phrase. A server mid-task with an active SSE stream has refcount ≥ 1. It cannot be killed by this logic, regardless of how long it's been since the last tool call.

This is a debounce against spawn/kill thrash, not an idle timeout. The mechanism is categorically different.

### Solution to Problem 3: STDIO refused at config load

mcprt's policy validator runs before any process is spawned. It hard-refuses specs that:

- Use process launchers (`npx`, `node`, `python`, `python3`, `deno`, `bun`) without explicit opt-in
- Match known STDIO MCP package patterns (`@modelcontextprotocol/server-*`, `mcp-server-*`, `-mcp-server`)
- Have no port binding argument — meaning they cannot be reverse-proxied and must be using STDIO

STDIO is not a configurable default. It is not a flag. It is not behind `--allow-stdio`. The policy validator treats it as a vulnerability finding, not a style preference. If you have a Python binary that genuinely binds an HTTP port, you set `acknowledged_stdio_safe = true` and get a logged warning confirming that's intentional.

```
$ mcprt validate ~/.config/mcprt/mcprt.toml

ERROR  server "my-server": exec uses "npx" — a process launcher that commonly
       wraps STDIO MCPs. If this binary genuinely starts an HTTP MCP server,
       set acknowledged_stdio_safe=true.
ERROR  server "my-server": arg "@modelcontextprotocol/server-filesystem" matches
       known STDIO MCP package pattern "@modelcontextprotocol/server-".
```

`mcprt validate` exits non-zero on errors. Wire it into your CI.

### Solution to Problem 4: Single binary, zero runtime deps

mcprt is one ~10 MB Go binary. No Docker. No Kubernetes. No Redis. No cloud account. It ships a macOS launchd plist and a systemd unit. If you use Homebrew, one command installs it.

The library (`pkg/runtime`, `pkg/proxy`, `pkg/supervisor`, `pkg/manifest`, `pkg/policy`) is importable. If you're building a Claude Code wrapper, a Cline plugin, or a custom dashboard, you embed the library and own the lifecycle.

---

## Architecture

```
Client (Claude Code, Cline, Continue)
        │
        ▼
mcprt proxy :9090
        │
        ├── /vault-mcp/...    ── RefCounter ── Supervisor ── vault-mcp :19000
        ├── /ga-surgifai/...  ── RefCounter ── Supervisor ── (idle, not spawned)
        └── /google-ads/...   ── RefCounter ── Supervisor ── (idle, not spawned)
```

- **Proxy** routes on the first path segment, strips it, and forwards to the upstream port.
- **RefCounter** tracks open SSE streams and ephemeral session IDs. Fires the stop callback after the grace period debounce when refcount reaches zero.
- **Supervisor** manages the process: `idle → spawning → running → stopping`. Health-checks via configurable HTTP path or 500ms fixed delay. SIGTERM → SIGKILL after grace window.
- **Runtime** owns port allocation, fsnotify hot-reload, and reconciliation diffs on config change.

---

## Quickstart

```sh
# Install (manual until Homebrew formula ships)
go install github.com/victorqnguyen/mcprt/cmd/mcprt@latest

# Write config
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

# Validate before starting anything
mcprt validate ~/.config/mcprt/mcprt.toml

# Run
mcprt serve

# Point your MCP client at it — Claude Code example:
# ~/.claude/mcp.json:
# {
#   "mcpServers": {
#     "vault-mcp": { "type": "http", "url": "http://localhost:9090/vault-mcp/mcp" },
#     "ga-mysite": { "type": "http", "url": "http://localhost:9090/ga-mysite/mcp" }
#   }
# }
```

**Run as a macOS service** (starts at login, stays out of your way):

```sh
# Edit dist/launchd/com.mcprt.daemon.plist — replace YOUR_USERNAME
cp dist/launchd/com.mcprt.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.mcprt.daemon.plist
```

---

## Manifest reference

```toml
[runtime]
listen       = "127.0.0.1:9090"   # proxy bind address
log_level    = "info"              # debug | info | warn | error
grace_period = "5s"                # debounce window before stopping idle server

[server.<name>]
exec                    = ["/abs/path/to/binary"]      # required
args                    = ["--port", "${MCPRT_PORT}"]  # ${MCPRT_PORT} → dynamic allocation
env                     = { KEY = "value" }            # merged with OS environment
health_path             = "/health"                    # HTTP GET probe; omit for 500ms delay
working_dir             = "/path/to/dir"
allow_external          = false   # allow non-loopback bind addresses
acknowledged_stdio_safe = false   # suppress STDIO launcher warning (document why)
```

`${MCPRT_PORT}` is the only magic variable. mcprt allocates a port per server starting at 19000 and substitutes it everywhere in `args` and `env`.

---

## CLI

| Command | Description |
|---|---|
| `mcprt serve` | Start the proxy daemon |
| `mcprt validate <file>` | Check a manifest; exits 1 on policy errors |
| `mcprt validate --json <file>` | Machine-readable violations list |
| `mcprt status` | Snapshot of all servers: state, PID, port, restart count |
| `mcprt status --json` | Same, as JSON |

Edits to `mcprt.toml` are picked up automatically while `serve` is running. New servers are added to the registry; removed servers are stopped; mutated running servers are restarted. Policy is re-validated on every reload — an invalid edit is logged and rejected without disrupting running servers.

---

## Embedding the library

mcprt is a Go library first. The CLI is one consumer. Build your own:

```go
import (
    "context"
    "github.com/victorqnguyen/mcprt/pkg/runtime"
    "github.com/victorqnguyen/mcprt/pkg/policy"
    "github.com/victorqnguyen/mcprt/pkg/manifest"
)

// Validate without starting anything
cfg, _ := manifest.Load("mcprt.toml")
violations := policy.Validate(cfg)

// Full runtime with hooks
rt, _ := runtime.New(runtime.Options{ManifestPath: "mcprt.toml"})
rt.OnSpawn = func(name string) { dashboard.SetRunning(name) }
rt.OnExit  = func(name string) { dashboard.SetIdle(name) }
rt.Serve(ctx)
```

Public API surface: `runtime.Runtime`, `manifest.Config`, `manifest.ServerSpec`, `proxy.Handler`, `proxy.RefCounter`, `supervisor.Supervisor`, `supervisor.Stats`, `policy.Validate`, `policy.Violation`.

---

## Why this exists

mcprt was built to solve a real hardware problem. A 16 GB Apple Silicon Mac Mini running five always-on MCP servers hit VM compressor saturation — 100% of compression segments, 45 swapfiles — and triggered kernel watchdog panics. The machine rebooted. Logs confirmed:

```
panic: watchdog timeout: no checkins from watchdogd in 93 seconds
Compressor Info: 100% of segments limit (BAD) with 45 swapfiles
```

Turning off the MCP servers reclaimed ~500 MB of idle RAM and eliminated the panics. The question was how to get that memory back without losing the servers when actually needed. Idle timeouts were wrong. Always-on was untenable. Connection-refcounting was the correct primitive — it had just never been built as a standalone tool for the MCP ecosystem.

---

## Security model

- All upstream servers bind `127.0.0.1` by default. External binding requires explicit `allow_external = true`.
- STDIO transport is refused unconditionally — not configurable, not behind a flag.
- The policy validator runs at startup and on every hot-reload. A bad manifest entry cannot slip through between reloads.
- mcprt does not store, log, or proxy credentials. Environment variables are passed to child processes as specified — same as launchd would.
- The proxy strips the `/<server-name>` path prefix before forwarding. The upstream server sees only its own routes.

---

## Roadmap

- [ ] `mcprt status --watch` — live TUI (bubbletea)
- [ ] `mcprt logs <server>` — tail per-server stdout/stderr
- [ ] `mcprt up <server>` / `mcprt down <server>` — manual overrides
- [ ] Prometheus `/metrics` endpoint (opt-in)
- [ ] Homebrew formula
- [ ] systemd unit (`dist/systemd/`)
- [ ] RSS sampling via gopsutil (already a dep, not yet wired to status output)

Explicitly out of scope: multi-host clustering, GUI dashboard, idle-timeout fallback, STDIO support of any kind.

---

## Contributing

Apache 2.0. PRs welcome. If you're adding a detection rule to the policy validator, include the CVE or disclosure link in the commit message. If you're adding a transport, it must be Streamable HTTP — STDIO PRs will be closed.

```
mcprt/
├── pkg/
│   ├── manifest/   TOML loader + schema
│   ├── policy/     Validator + violation types
│   ├── proxy/      HTTP reverse proxy + RefCounter
│   ├── supervisor/ Process lifecycle
│   └── runtime/    Orchestrator + hot-reload
├── cmd/mcprt/      CLI entry point
├── dist/           Platform integration (launchd, systemd, homebrew)
└── examples/       mcprt.toml + mcp.json samples
```
