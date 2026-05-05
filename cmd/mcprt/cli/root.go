// Package cli wires together all mcprt subcommands.
package cli

import "github.com/spf13/cobra"

// Root returns the top-level cobra command.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "mcprt",
		Short: "Connection-refcounted MCP runtime — servers run iff a client is talking to them",
		Long: `mcprt manages MCP servers on demand. A server starts the moment a client
connects and stops (with a brief debounce) the moment the last client disconnects.
No idle timeouts. No always-on resident processes. STDIO transport refused.`,
	}
	root.AddCommand(validateCmd())
	root.AddCommand(serveCmd())
	root.AddCommand(statusCmd())
	return root
}
