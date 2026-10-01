package cmd

import (
	"slices"
	"sort"
	"strings"

	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/apiprotocol"
)

// MCPCommandDescriptors returns detached metadata captured before the daemon
// starts serving. Cobra traversal mutates flag caches and is not safe in requests.
func (a *storeAPIAdapter) MCPCommandDescriptors() []apiprotocol.MCPCommandDescriptor {
	descriptors := slices.Clone(a.mcpCommands)
	for i := range descriptors {
		descriptors[i].Flags = slices.Clone(descriptors[i].Flags)
	}
	return descriptors
}

func registeredMCPCommandDescriptors() []apiprotocol.MCPCommandDescriptor {
	paths := [][]string{
		{"draft-reply"}, {"draft-compose"}, {"draft-forward"}, {"draft-get"},
		{"draft-list"}, {"draft-edit"}, {"draft-delete"}, {"draft-recover"}, {"draft-send-as"},
		{"person", "provider", multimodalStatusSubcommand}, {"person", "provider", "history"},
		{documentsCommandName, "policy"}, {documentsCommandName, "consent-mistral"},
		{documentsCommandName, "build"}, {documentsCommandName, "resume"}, {documentsCommandName, "retry"},
	}
	descriptors := make([]apiprotocol.MCPCommandDescriptor, 0, len(paths))
	for _, path := range paths {
		command, remaining, err := rootCmd.Find(path)
		if err != nil || len(remaining) != 0 || command.Name() != path[len(path)-1] || command.RunE == nil {
			continue
		}
		descriptor := apiprotocol.MCPCommandDescriptor{
			Name: strings.Join(path, " "), Flags: []string{}, Delegated: agentDelegatedCapable(command),
		}
		command.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
			descriptor.Flags = append(descriptor.Flags, flag.Name)
		})
		sort.Strings(descriptor.Flags)
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	return descriptors
}
