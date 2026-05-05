// Package policy validates manifest configs against mcprt's security posture.
// Key stance: Streamable HTTP only — STDIO transport is refused unconditionally.
package policy

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/victorqnguyen/mcprt/pkg/manifest"
)

// Severity of a policy violation.
type Severity string

const (
	SeverityError   Severity = "error"   // hard refusal — server will not load
	SeverityWarning Severity = "warning" // informational — server loads with a log entry
)

// Violation describes a single policy breach.
type Violation struct {
	Server   string   // "" for runtime-level violations
	Rule     string   // machine-readable rule ID
	Severity Severity
	Message  string
}

func (v Violation) String() string {
	if v.Server != "" {
		return fmt.Sprintf("[%s] server %q: %s", v.Severity, v.Server, v.Message)
	}
	return fmt.Sprintf("[%s] runtime: %s", v.Severity, v.Message)
}

// Validate runs all policy checks on cfg and returns any violations.
// Errors are hard blocks; warnings are surfaced but do not prevent load.
func Validate(cfg *manifest.Config) []Violation {
	var vv []Violation
	vv = append(vv, validateRuntime(cfg)...)
	for name, spec := range cfg.Server {
		vv = append(vv, validateServer(name, spec)...)
	}
	return vv
}

// HasErrors returns true if any violation has severity Error.
func HasErrors(vv []Violation) bool {
	for _, v := range vv {
		if v.Severity == SeverityError {
			return true
		}
	}
	return false
}

func validateRuntime(cfg *manifest.Config) []Violation {
	var vv []Violation
	if cfg.Runtime.Listen == "" {
		vv = append(vv, Violation{
			Rule: "runtime.listen.empty", Severity: SeverityError,
			Message: "runtime.listen must be set",
		})
	}
	return vv
}

func validateServer(name string, spec manifest.ServerSpec) []Violation {
	var vv []Violation

	// --- STDIO detection (hard error) ---

	if len(spec.Exec) == 0 {
		vv = append(vv, Violation{
			Server: name, Rule: "server.exec.empty", Severity: SeverityError,
			Message: "exec must have at least one element",
		})
		return vv // nothing more to check
	}

	binary := filepath.Base(spec.Exec[0])

	// Executables that are process launchers, not HTTP servers.
	stdioLaunchers := []string{"node", "npx", "python", "python3", "deno", "bun"}
	for _, launcher := range stdioLaunchers {
		if binary == launcher {
			if !spec.AcknowledgedStdioSafe {
				vv = append(vv, Violation{
					Server: name, Rule: "server.stdio.launcher", Severity: SeverityError,
					Message: fmt.Sprintf(
						"exec uses %q — a process launcher that commonly wraps STDIO MCPs. "+
							"If this binary genuinely starts an HTTP MCP server, set acknowledged_stdio_safe=true.",
						binary,
					),
				})
			} else {
				vv = append(vv, Violation{
					Server: name, Rule: "server.stdio.launcher.acknowledged", Severity: SeverityWarning,
					Message: fmt.Sprintf("exec uses %q with acknowledged_stdio_safe=true — ensure it binds an HTTP port", binary),
				})
			}
		}
	}

	// Known STDIO-wrapper package names in npx/uvx invocations.
	stdioPackageFragments := []string{
		"@modelcontextprotocol/server-",
		"mcp-server-",
		"-mcp-server",
	}
	allArgs := append(spec.Exec[1:], spec.Args...)
	for _, arg := range allArgs {
		for _, frag := range stdioPackageFragments {
			if strings.Contains(arg, frag) && !spec.AcknowledgedStdioSafe {
				vv = append(vv, Violation{
					Server: name, Rule: "server.stdio.package", Severity: SeverityError,
					Message: fmt.Sprintf(
						"arg %q matches known STDIO MCP package pattern %q. "+
							"If this package exposes an HTTP interface, set acknowledged_stdio_safe=true.",
						arg, frag,
					),
				})
			}
		}
	}

	// --- Port binding check ---
	// A server that doesn't bind a port can't be reverse-proxied. We require
	// either ${MCPRT_PORT} in args or a literal --port / -port flag.
	if !hasPortBinding(spec) {
		vv = append(vv, Violation{
			Server: name, Rule: "server.no_port", Severity: SeverityError,
			Message: "server args do not contain ${MCPRT_PORT} or a --port/--bind flag; " +
				"mcprt cannot determine where to proxy requests",
		})
	}

	// --- Host restriction ---
	if !spec.AllowExternal {
		// Check exec path doesn't bind 0.0.0.0 via obvious flag patterns.
		for _, arg := range allArgs {
			if arg == "0.0.0.0" || arg == "::0" {
				vv = append(vv, Violation{
					Server: name, Rule: "server.external_bind", Severity: SeverityError,
					Message: fmt.Sprintf(
						"arg %q binds all interfaces. Set allow_external=true if this is intentional.", arg,
					),
				})
			}
		}
	}

	return vv
}

// hasPortBinding returns true if the server spec contains a port substitution
// token or an explicit --port / -p flag in its arguments.
func hasPortBinding(spec manifest.ServerSpec) bool {
	allArgs := append(spec.Exec[1:], spec.Args...)
	for i, arg := range allArgs {
		if strings.Contains(arg, "${MCPRT_PORT}") {
			return true
		}
		// Accept --port <N>, --port=<N>, -p <N>
		if arg == "--port" || arg == "-p" || arg == "--bind" {
			if i+1 < len(allArgs) {
				return true
			}
		}
		if strings.HasPrefix(arg, "--port=") || strings.HasPrefix(arg, "--bind=") {
			return true
		}
	}
	return false
}
