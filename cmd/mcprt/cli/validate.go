package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/victorqnguyen/mcprt/pkg/manifest"
	"github.com/victorqnguyen/mcprt/pkg/policy"
)

func validateCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "validate <manifest>",
		Short: "Validate a mcprt.toml manifest against policy rules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := manifest.Load(args[0])
			if err != nil {
				return fmt.Errorf("loading manifest: %w", err)
			}

			violations := policy.Validate(cfg)

			if jsonOut {
				return printViolationsJSON(violations)
			}

			return printViolationsText(violations, args[0])
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output violations as JSON")
	return cmd
}

func printViolationsText(violations []policy.Violation, path string) error {
	if len(violations) == 0 {
		fmt.Printf("OK  %s — no policy violations\n", path)
		return nil
	}

	errors, warnings := 0, 0
	for _, v := range violations {
		switch v.Severity {
		case policy.SeverityError:
			fmt.Fprintf(os.Stderr, "ERROR   %s\n", v)
			errors++
		case policy.SeverityWarning:
			fmt.Fprintf(os.Stderr, "WARN    %s\n", v)
			warnings++
		}
	}

	fmt.Fprintf(os.Stderr, "\n%s: %d error(s), %d warning(s)\n", path, errors, warnings)

	if errors > 0 {
		return fmt.Errorf("manifest has %d policy error(s)", errors)
	}
	return nil
}

func printViolationsJSON(violations []policy.Violation) error {
	// Manual JSON output to avoid importing encoding/json for a simple struct.
	fmt.Println("[")
	for i, v := range violations {
		comma := ","
		if i == len(violations)-1 {
			comma = ""
		}
		fmt.Printf(`  {"server":%q,"rule":%q,"severity":%q,"message":%q}%s`+"\n",
			v.Server, v.Rule, v.Severity, v.Message, comma)
	}
	fmt.Println("]")

	if policy.HasErrors(violations) {
		return fmt.Errorf("manifest has policy errors")
	}
	return nil
}
