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

If you need to build an MCP server, use a fork of the skill that enforces Streamable HTTP from the start: [victorqnguyen/skills — mcp-builder](https://github.com/victorqnguyen/skills/tree/main/skills/mcp-builder).

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

## Stress test results

Concurrent cold-start of 4 MCP servers (vault-mcp, surgifai-coderag, Google Analytics, Google Ads) on a Mac Mini M2, sampled every 3 seconds.

```
timestamp   mcprt    vault     coderag   GA        Ads      children
─────────────────────────────────────────────────────────────────────
17:12:51    16.3 MB   0.0 MB    0.0 MB   0.0 MB   0.0 MB   0   ← idle
17:12:54    16.3 MB   0.0 MB    0.0 MB   0.0 MB   0.0 MB   0
17:13:07    16.3 MB   0.0 MB    0.0 MB   0.0 MB   0.0 MB   1   ← first spawn
17:13:10    16.5 MB  121.0 MB  233.9 MB  0.0 MB   0.0 MB   2
17:13:13    16.5 MB   0.0 MB   235.2 MB 142.9 MB 143.5 MB  3   ← peak load
17:13:16    16.6 MB   0.0 MB   235.2 MB 142.9 MB 143.8 MB  3
17:13:20    16.6 MB   0.0 MB    0.0 MB   0.0 MB   0.0 MB   0   ← all idle
17:13:23    16.6 MB   0.0 MB    0.0 MB   0.0 MB   0.0 MB   0
```

**mcprt daemon idle RSS: 16.6 MB.** At peak concurrent load across 4 servers the daemon itself grew by less than 1 MB — all the memory is in the child processes, which are reclaimed within `grace_period` of the last client disconnecting.

State machine log during the same run:

```
17:13:07  INFO  cold start triggered          server=vault-mcp
17:13:07  INFO  state change  idle→spawning   server=vault-mcp
17:13:08  INFO  state change  spawning→running server=vault-mcp   ← health passed in 800ms
17:13:09  INFO  cold start triggered          server=surgifai-coderag
17:13:09  INFO  state change  idle→spawning   server=surgifai-coderag
17:13:11  INFO  state change  spawning→running server=surgifai-coderag
17:13:11  INFO  cold start triggered          server=ga-vyledentistry
17:13:11  INFO  state change  idle→spawning   server=ga-vyledentistry
17:13:12  INFO  state change  spawning→running server=ga-vyledentistry
17:13:17  INFO  refcount debounce expired, stopping server=ga-vyledentistry
17:13:17  INFO  sending SIGTERM               server=ga-vyledentistry
17:13:17  INFO  state change  running→stopping server=ga-vyledentistry
17:13:18  INFO  process exited                server=ga-vyledentistry  err=<nil>
17:13:18  INFO  state change  stopping→idle   server=ga-vyledentistry
```

Server responses confirmed during the run:

| Server | Status | Version | Tools |
|---|---|---|---|
| vault-mcp | ✅ | vault_mcp 1.27.0 | 3 (query, fetch, stats) |
| surgifai-coderag | ✅ | surgifai-coderag 0.1.0 | 36 |
| ga-vyledentistry | ✅ | Google Analytics MCP | — |
| google-ads | ✅ | Google Ads Server 3.2.4 | — |

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

### Install

**Homebrew (macOS / Linux — recommended)**

```sh
brew install surgifai-com/tap/mcprt
```

**Go install**

```sh
# Requires Go 1.21+
go install github.com/surgifai-com/mcprt/cmd/mcprt@latest
```

**Binary**

Download a pre-built archive for your platform from [GitHub Releases](https://github.com/surgifai-com/mcprt/releases), extract, and move `mcprt` to your `$PATH`.

---

### Greenfield — first MCP server, starting fresh

You have an MCP server binary and want mcprt to manage it. You haven't set anything up yet.

**1. Write the config**

```sh
mkdir -p ~/.config/mcprt
```

```toml
# ~/.config/mcprt/mcprt.toml
[runtime]
listen       = "127.0.0.1:9090"
grace_period = "5s"

[server.my-mcp]
exec        = ["/path/to/.venv/bin/my-mcp-server"]
args        = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
health_type = "tcp"
```

**2. Validate and start**

```sh
mcprt validate ~/.config/mcprt/mcprt.toml
mcprt serve
```

**3. Point your AI client at mcprt**

Claude Code (`~/.claude/mcp.json`):

```json
{
  "mcpServers": {
    "my-mcp": { "type": "http", "url": "http://localhost:9090/my-mcp/mcp" }
  }
}
```

That's it. The server starts the first time Claude Code connects and stops when it disconnects (after `grace_period`).

**4. Run as a background service (macOS)**

```sh
# Substitute YOUR_USERNAME in the plist, then:
cp dist/launchd/com.mcprt.daemon.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.mcprt.daemon.plist
```

> **macOS + external volumes:** If any server binary lives on an external drive (`/Volumes/...`), grant Full Disk Access to `mcprt` in System Settings → Privacy & Security → Full Disk Access. See [Troubleshooting](#troubleshooting).

---

### Brownfield — migrating existing always-on servers

You already have MCP servers running — launchd plists, shell scripts, or entries in `mcp.json` with `command`/`args` (STDIO). You want to stop managing them manually and let mcprt handle lifecycle.

**1. Inventory what you have**

```sh
# launchd (macOS)
ls ~/Library/LaunchAgents/ | grep mcp

# Claude Desktop / Claude Code STDIO entries
cat ~/.claude/mcp.json        # look for "command" / "args" keys — those are STDIO
cat ~/Library/Application\ Support/Claude/claude_desktop_config.json
```

**2. For each server, find the binary and port pattern**

- If the entry has `"command": "npx"` or `"command": "python"` — it's STDIO. You need to check whether the server supports Streamable HTTP; if not, it cannot run under mcprt.
- If the server already binds an HTTP port, note how it accepts it: `--port`, `-p`, or an env var like `PORT`.

**3. Write the mcprt manifest**

```toml
# ~/.config/mcprt/mcprt.toml
[runtime]
listen       = "127.0.0.1:9090"
grace_period = "5s"

# Server that takes --port as a CLI arg
[server.my-existing-mcp]
exec        = ["/path/to/existing-mcp-binary"]
args        = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
health_type = "tcp"
health_timeout = "15s"   # raise if startup is slow

# Server that reads port from an env var
[server.my-env-port-mcp]
exec        = ["/path/to/.venv/bin/python", "-m", "my_mcp.server"]
working_dir = "/path/to/project"
env         = { PORT = "${MCPRT_PORT}" }
health_type = "tcp"
health_timeout = "15s"
acknowledged_stdio_safe = true   # python binary confirmed to bind HTTP
```

**4. Validate**

```sh
mcprt validate ~/.config/mcprt/mcprt.toml
```

The validator will flag any spec that looks like STDIO. Fix those before proceeding.

**5. Stop the always-on services**

```sh
# launchd
launchctl unload ~/Library/LaunchAgents/com.myserver.plist

# Or disable KeepAlive in the plist and reload
```

**6. Update your AI client config**

Replace `"command"`/`"args"` STDIO entries with mcprt HTTP URLs:

```json
{
  "mcpServers": {
    "my-existing-mcp": { "type": "http", "url": "http://localhost:9090/my-existing-mcp/mcp" },
    "my-env-port-mcp": { "type": "http", "url": "http://localhost:9090/my-env-port-mcp/mcp" }
  }
}
```

**7. Start mcprt and verify**

```sh
mcprt serve
mcprt status   # should show all servers idle, 0 MB
```

Connect from your AI client — `mcprt status` should show each server transition `idle → running` on first use and back to `idle` after disconnect.

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
    "github.com/surgifai-com/mcprt/pkg/manifest"
    "github.com/surgifai-com/mcprt/pkg/policy"
    "github.com/surgifai-com/mcprt/pkg/runtime"
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
- [x] Homebrew formula (`surgifai-com/tap`)
- [ ] systemd unit (`dist/systemd/`)
- [ ] RSS + CPU sampling in `mcprt status` (gopsutil already a dep)

**Explicitly out of scope for v1:** multi-host clustering, web/GUI dashboard, idle-timeout fallback mode, STDIO support of any kind. These are not future features. They are non-goals.

---

## Troubleshooting

### macOS: vault-mcp hangs at startup, health check times out

**Symptom:** `spawn failed: health check for "vault-mcp": context deadline exceeded`. The vault-mcp process appears in `ps aux` but never binds its port. `sample <pid>` shows Python stuck in `__open_nocancel` during `Py_InitializeFromConfig`.

**Cause:** macOS TCC (privacy controls) blocks launchd-spawned processes from accessing external volumes (`/Volumes/...`) until Full Disk Access is granted. The `open()` syscall hangs indefinitely waiting for user consent. This does not affect terminal-launched processes because Terminal.app already holds FDA and child processes inherit access.

**Fix:** System Settings → Privacy & Security → Full Disk Access → `+` → navigate to `~/.local/bin/mcprt` → toggle on. Then restart the daemon:

```bash
launchctl kickstart -k gui/$(id -u)/com.victor.mcprt
```

This applies to any mcprt-managed server whose binary lives outside `~/`. If you move servers to `~/` or a system path, FDA is not required.

### Health check times out but the server process is running

**Symptom:** `spawn failed: health check ... context deadline exceeded`. The server process appears in `ps aux`, is binding its port, and works when started manually — but mcprt gives up before it's ready.

**Cause:** The default health check timeout is 5 seconds. Some servers (Python with heavy imports, Node.js with large dependency trees, anything loading ML models or doing OAuth init at startup) take longer. The 5s default fires before the process has finished initializing.

**Diagnosis:**

```bash
# While the error is happening, check if the process IS running:
ps aux | grep <server-name>

# Check what port it bound (if any):
lsof -i :<expected-port>
```

If the process shows up in `ps` and the port is bound, the issue is purely timing.

**Fix:** Add `health_timeout` and `health_type = "tcp"` to the server spec:

```toml
[server.my-slow-server]
exec           = ["/path/to/binary"]
args           = ["--port", "${MCPRT_PORT}"]
health_type    = "tcp"
health_timeout = "20s"   # raise until cold-start consistently passes
```

Use `"tcp"` health type when the server doesn't expose an HTTP health endpoint — it just checks that the port is accepting connections, which happens earlier in startup than HTTP handlers being ready.

**Rule of thumb for timeout values:**

| Server type | Suggested `health_timeout` |
|---|---|
| Simple HTTP server | `5s` (default) |
| Python with heavy imports (FastAPI, ADK, etc.) | `15s`–`20s` |
| Node.js with large dependency tree | `10s`–`15s` |
| Python loading ML model at startup | `30s`–`60s` |

---

---

Built by [@victorqnguyen](https://github.com/victorqnguyen) · [surgifai-com](https://github.com/surgifai-com)

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
├── dist/           launchd plist template (systemd coming)
└── examples/       mcprt.toml + mcp.json for Claude Code
```
