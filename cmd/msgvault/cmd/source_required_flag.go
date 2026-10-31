package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"strings"
)

func sourceRequiredString(cmd *cobra.Command, name, meaning, example string) error {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return fmt.Errorf("read --%s: %w", name, err)
	}
	if strings.TrimSpace(value) == "" {
		return withHint(usageErr(cmd, fmt.Errorf("--%s is required: %s", name, meaning)), "for example: "+example)
	}
	return nil
}
