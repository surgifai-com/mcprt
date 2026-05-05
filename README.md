# mcprt

**Connection-refcounted MCP runtime.** MCP servers run iff a client is talking to them — not before, not after, no timeouts.

---

## The problem

Every Claude Code / Cline / Continue user running ≥3 local MCP servers faces the same math:

| Setup | Idle RAM |
|---|---|
| 4× always-on launchd MCP services | ~500 MB |
| mcprt daemon + those same 4 servers with no active client | ~30 MB |

Existing alternatives miss:
- **Idle-timeout eviction** — wrong heuristic. Aggressive timeouts kill mid-task; lax timeouts don't save memory.
- **Heavyweight orchestration** (kubernetes, redis) — wrong scale for a laptop.

mcprt's trigger is **refcount, not timeout.** A server spawns the moment a client connects. It shuts down (after a short debounce) the moment the last client disconnects. Between sessions: zero resident MCP processes.

---

## Quickstart

```sh
# 1. Install
brew install mcprt           # coming soon — for now: go install github.com/victorqnguyen/mcprt/cmd/mcprt@latest

# 2. Write ~/.config/mcprt/mcprt.toml
[runtime]
listen = "127.0.0.1:9090"

[server.vault-mcp]
exec   = ["/path/to/vault-mcp/.venv/bin/vault-mcp"]
args   = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
health_path = "/health"

[server.ga-mysite]
exec = ["/path/to/analytics-mcp/.venv/bin/analytics-mcp"]
args = ["--port", "${MCPRT_PORT}", "--host", "127.0.0.1"]
env  = { GOOGLE_APPLICATION_CREDENTIALS = "~/.config/mcprt/secrets/mysite.json" }

# 3. Validate (catches STDIO violations before anything runs)
mcprt validate ~/.config/mcprt/mcprt.toml

# 4. Run
mcprt serve

# 5. Point your MCP client at it
# ~/.claude/mcp.json:
# { "mcpServers": { "vault-mcp": { "type": "http", "url": "http://localhost:9090/vault-mcp/mcp" } } }
```

---

## Transport policy

**STDIO transport is refused unconditionally.** This is not a configurable default; it's the product's identity.

mcprt was designed in response to the April 2026 OX Security disclosure (14 CVEs, 200K+ affected servers, 150M+ downloads — LiteLLM, LangChain, LangFlow, Flowise, LettaAI, LangBot). The root cause: MCP runtimes that treat `exec` as a first-class transport, letting an untrusted manifest spawn arbitrary processes with ambient credential access.

`mcprt validate` refuses any spec that:
- Uses `npx`, `node`, `python`, `python3`, `deno`, or `bun` as the binary without `acknowledged_stdio_safe = true`
- Matches known STDIO MCP package patterns (`@modelcontextprotocol/server-*`, `mcp-server-*`)
- Has no port binding argument (`${MCPRT_PORT}`, `--port`, `--bind`)

Set `acknowledged_stdio_safe = true` only when you've verified the binary genuinely binds an HTTP port.

---

## How it works

```
Client (Claude Code)  ──→  mcprt :9090  ──→  /<server-name>/...
                                │
                     RefCounter (SSE streams + Mcp-Session-Id)
                                │
                     Supervisor (spawn, health-check, SIGTERM, SIGKILL)
                                │
                     MCP server process on :19000+N
```

**Refcount semantics:**
- Open SSE stream → `refcount++`
- SSE stream closed → `refcount--`
- `Mcp-Session-Id` POST (no SSE) → ephemeral ref, released on TCP close
- `refcount` transitions `0→1` → spawn
- `refcount` stable at 0 for `grace_period` (default 5s) → SIGTERM → SIGKILL after 5s

This is a *debounce*, not an idle timeout. Idle timeout: "not used in N seconds." Debounce: "no connections open for N seconds." Different failure modes.

---

## Manifest reference

```toml
[runtime]
listen       = "127.0.0.1:9090"   # proxy bind address
log_level    = "info"              # debug | info | warn | error
grace_period = "5s"                # debounce before stopping idle server

[server.<name>]
exec         = ["/path/to/binary"]           # required; first element is the executable
args         = ["--port", "${MCPRT_PORT}"]   # ${MCPRT_PORT} → dynamic allocation
env          = { KEY = "value" }             # merged with OS environment
health_path  = "/health"                     # GET probe; omit to skip (uses 500ms delay)
working_dir  = "/path/to/working/dir"
allow_external      = false   # allow binding non-loopback interfaces
acknowledged_stdio_safe = false  # suppress STDIO launcher warning
```

---

## CLI

| Command | Description |
|---|---|
| `mcprt serve` | Start the daemon |
| `mcprt validate <file>` | Validate a manifest; non-zero exit on errors |
| `mcprt status` | Show state, PID, port, restart count for all servers |
| `mcprt status --json` | Machine-readable output |

Hot-reload is automatic — edit `mcprt.toml` while `serve` is running.

---

## Library

```go
import "github.com/victorqnguyen/mcprt/pkg/runtime"

rt, err := runtime.New(runtime.Options{
    ManifestPath: "~/.config/mcprt/mcprt.toml",
})
rt.OnSpawn = func(name string) { /* dashboard update */ }
rt.OnExit  = func(name string) { /* dashboard update */ }
rt.Serve(ctx)
```

---

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design rationale, refcount correctness analysis, and HTTP/2 multiplexing notes.

---

## License

MIT
