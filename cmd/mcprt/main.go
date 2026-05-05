// Command mcprt — connection-refcounted MCP runtime daemon and CLI.
package main

import (
	"fmt"
	"os"

	"github.com/victorqnguyen/mcprt/cmd/mcprt/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mcprt: %v\n", err)
		os.Exit(1)
	}
}
