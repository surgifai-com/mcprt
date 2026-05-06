// Package manifest loads and parses mcprt.toml configuration files.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level mcprt.toml structure.
type Config struct {
	Runtime RuntimeConfig        `toml:"runtime"`
	Server  map[string]ServerSpec `toml:"server"`
}

// RuntimeConfig holds daemon-wide settings.
type RuntimeConfig struct {
	Listen      string `toml:"listen"`
	LogLevel    string `toml:"log_level"`
	GracePeriod string `toml:"grace_period"`
}

// ServerSpec defines a single managed MCP server.
type ServerSpec struct {
	Exec              []string          `toml:"exec"`
	Args              []string          `toml:"args"`
	Env               map[string]string `toml:"env"`
	HealthPath        string            `toml:"health_path"`
	WorkingDir        string            `toml:"working_dir"`
	AllowExternal     bool              `toml:"allow_external"`
	AcknowledgedStdioSafe bool          `toml:"acknowledged_stdio_safe"`
	// Port pins the upstream port instead of using dynamic allocation.
	// Use this for servers that read their port from a config file rather
	// than a CLI argument. When set, ${MCPRT_PORT} substitution still works
	// but is not required by the policy validator.
	Port int `toml:"port"`
	// HealthTimeout overrides the global health-check deadline for this server.
	// Useful for servers with slow startup (e.g. loading large ML models).
	// Accepts Go duration strings: "30s", "1m", "500ms". Default: 5s.
	HealthTimeout string `toml:"health_timeout"`
	// HealthType controls the probe mechanism. Options:
	//   "http" (default): GET health_path, pass if status < 500
	//   "tcp":  dial the port, pass when connection is accepted (no HTTP)
	//   "none": skip health check entirely, use a 500ms fixed delay
	HealthType string `toml:"health_type"`
}

// DefaultRuntimeConfig returns conservative defaults.
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		Listen:      "127.0.0.1:9090",
		LogLevel:    "info",
		GracePeriod: "5s",
	}
}

// Load parses a mcprt.toml file and returns the config.
func Load(path string) (*Config, error) {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %q: %w", path, err)
	}
	return parse(data)
}

// Parse parses raw TOML bytes — exported so policy.Validate can be called
// without touching the filesystem.
func Parse(data []byte) (*Config, error) {
	return parse(data)
}

func parse(data []byte) (*Config, error) {
	cfg := &Config{
		Runtime: DefaultRuntimeConfig(),
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	// Expand ~ in each server's working dir and exec paths.
	for name, spec := range cfg.Server {
		if len(spec.Exec) > 0 {
			spec.Exec[0] = expandHome(spec.Exec[0])
		}
		spec.WorkingDir = expandHome(spec.WorkingDir)
		cfg.Server[name] = spec
	}
	cfg.Runtime.Listen = expandHome(cfg.Runtime.Listen)
	return cfg, nil
}

func expandHome(s string) string {
	if !strings.HasPrefix(s, "~/") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, s[2:])
}
