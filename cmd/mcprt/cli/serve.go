package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/surgifai-com/mcprt/pkg/runtime"
)

func serveCmd() *cobra.Command {
	var (
		manifestPath string
		logDir       string
		logLevel     string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the mcprt daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			lvl := slog.LevelInfo
			switch logLevel {
			case "debug":
				lvl = slog.LevelDebug
			case "warn":
				lvl = slog.LevelWarn
			case "error":
				lvl = slog.LevelError
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

			rt, err := runtime.New(runtime.Options{
				ManifestPath: manifestPath,
				Logger:       logger,
				LogDir:       logDir,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return rt.Serve(ctx)
		},
	}

	home, _ := os.UserHomeDir()
	cmd.Flags().StringVar(&manifestPath, "config", filepath.Join(home, ".config", "mcprt", "mcprt.toml"), "manifest path")
	cmd.Flags().StringVar(&logDir, "log-dir", filepath.Join(home, ".local", "state", "mcprt", "logs"), "server log directory")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug|info|warn|error)")
	return cmd
}
