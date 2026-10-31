package cmd

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliJSONRequested follows pflag's value consumption, even after a parse error.
// Only commands declaring a JSON output flag can request JSON diagnostics.
func cliJSONRequested(cmd *cobra.Command, args []string) bool {
	if cmd == nil || cmd.Flags().Lookup("json") == nil {
		return false
	}
	jsonFlag := cmd.Flags().Lookup("json")
	if len(args) == 0 {
		value, _ := cmd.Flags().GetBool("json")
		return value
	}
	value, _ := strconv.ParseBool(jsonFlag.DefValue)
	selectValue := func(flag *pflag.Flag, raw string) {
		if flag.Name == "json" {
			if parsed, err := strconv.ParseBool(raw); err == nil {
				value = parsed
			}
		}
	}
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			break
		}
		if strings.HasPrefix(token, "--") {
			name, raw, assigned := strings.Cut(token[2:], "=")
			flag := cmd.Flags().Lookup(name)
			if flag == nil {
				continue
			}
			if !assigned {
				if flag.NoOptDefVal != "" {
					raw = flag.NoOptDefVal
				} else if i+1 < len(args) {
					i++
					raw = args[i]
				}
			}
			selectValue(flag, raw)
			continue
		}
		if !strings.HasPrefix(token, "-") || len(token) < 2 {
			continue
		}
		cluster := token[1:]
		for len(cluster) > 0 {
			// Shorthands are single ASCII bytes; an unknown prefix ends this cluster.
			if cluster[0] >= 128 {
				break
			}
			flag := cmd.Flags().ShorthandLookup(cluster[:1])
			if flag == nil {
				break
			}
			cluster = cluster[1:]
			if strings.HasPrefix(cluster, "=") {
				selectValue(flag, cluster[1:])
				break
			}
			if flag.NoOptDefVal != "" {
				selectValue(flag, flag.NoOptDefVal)
				continue
			}
			if len(cluster) > 0 {
				selectValue(flag, cluster)
			} else if i+1 < len(args) {
				i++
				selectValue(flag, args[i])
			}
			break
		}
	}
	return value
}
