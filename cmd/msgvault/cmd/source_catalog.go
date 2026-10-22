package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/sourcecatalog"
)

// Validate the compiled CLI registration tree before the daemon uses the
// catalog. A newly registered sync/import command cannot silently disappear
// from source discovery.
func nativeSourceInventory(root *cobra.Command) ([]sourcecatalog.Descriptor, error) {
	seen := make(map[string]bool)
	for _, command := range root.Commands() {
		name := command.Name()
		if name != "sync" && !strings.HasPrefix(name, "sync-") && !strings.HasPrefix(name, "import-") {
			continue
		}
		descriptors := sourcecatalog.ForCommand(name)
		if len(descriptors) == 0 || command.RunE == nil {
			return nil, fmt.Errorf("native source command %q is not registered in the source catalog", name)
		}
		if flag := command.Flags().Lookup("source-type"); flag != nil {
			provenance := sourcecatalog.Imported(flag.DefValue)
			if !slices.ContainsFunc(descriptors, func(d sourcecatalog.Descriptor) bool { return d.Type == provenance.Type && d.Imports }) {
				return nil, fmt.Errorf("native source command %q has an unregistered default import type", name)
			}
		}
		for _, descriptor := range descriptors {
			seen[descriptor.Type] = true
		}
	}
	result := sourcecatalog.List()
	for _, descriptor := range result {
		if len(sourcecatalog.Commands(descriptor.Type)) > 0 && !seen[descriptor.Type] {
			return nil, fmt.Errorf("native source type %q has no registered command", descriptor.Type)
		}
	}
	return result, nil
}

func sourceSupportsCommand(sourceType, command string) bool {
	return slices.ContainsFunc(sourcecatalog.ForCommand(command), func(d sourcecatalog.Descriptor) bool { return d.Type == sourceType })
}
