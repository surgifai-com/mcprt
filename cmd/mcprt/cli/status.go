package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/surgifai-com/mcprt/pkg/manifest"
	"github.com/surgifai-com/mcprt/pkg/supervisor"
)

func statusCmd() *cobra.Command {
	var (
		jsonOut      bool
		manifestPath string
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status of all managed servers",
		Long: `Reads the manifest and reports the last-known state of each server.
When the daemon is running, state is live; when not, it shows config only.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := manifest.Load(manifestPath)
			if err != nil {
				return err
			}

			// Build stub stats from manifest (we'll enhance with live daemon IPC in a later phase).
			stats := make([]supervisor.Stats, 0, len(cfg.Server))
			for name, spec := range cfg.Server {
				_ = spec
				stats = append(stats, supervisor.Stats{
					Name:  name,
					State: supervisor.StateIdle,
				})
			}

			if jsonOut {
				return json.NewEncoder(os.Stdout).Encode(stats)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVER\tSTATE\tPID\tPORT\tRESTARTS\tLAST SPAWN\tERRORS")
			for _, s := range stats {
				spawn := "-"
				if !s.LastSpawn.IsZero() {
					spawn = s.LastSpawn.Format(time.RFC3339)
				}
				pid := "-"
				if s.PID > 0 {
					pid = fmt.Sprintf("%d", s.PID)
				}
				port := "-"
				if s.Port > 0 {
					port = fmt.Sprintf("%d", s.Port)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d\n",
					s.Name, s.State, pid, port, s.RestartCount, spawn, s.Errors)
			}
			return w.Flush()
		},
	}

	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&manifestPath, "config", filepath.Join(home, ".config", "mcprt", "mcprt.toml"), "manifest path")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output as JSON")
	return cmd
}
