package cmd

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/apiprotocol"
)

func TestMCPCommandDescriptorsUseRegisteredCommandsAndAdmission(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	descriptors := (&storeAPIAdapter{mcpCommands: registeredMCPCommandDescriptors()}).MCPCommandDescriptors()
	byName := make(map[string]apiprotocol.MCPCommandDescriptor)
	for _, descriptor := range descriptors {
		byName[descriptor.Name] = descriptor
	}
	for _, name := range []string{"draft-reply", "draft-compose", "draft-get", "draft-edit", "draft-delete", "draft-recover", "draft-send-as"} {
		requirements.Contains(byName, name)
		command, _, err := rootCmd.Find([]string{name})
		requirements.NoError(err)
		requirements.Equal(name, command.Name())
		assertions.Equal(agentDelegatedCapable(command), byName[name].Delegated)
	}
	assertions.Contains(byName["draft-compose"].Flags, "to")
	assertions.Contains(byName["draft-compose"].Flags, "body")
	assertions.Contains(byName["draft-compose"].Flags, "source-id")
	assertions.NotContains(byName["draft-compose"].Flags, "agent-token-file")
	assertions.NotContains(byName, "sync")
	assertions.NotContains(byName, "add-account")
}

func TestMCPCommandDescriptorsConcurrentDetached(t *testing.T) {
	assertions := assert.New(t)
	adapter := &storeAPIAdapter{mcpCommands: registeredMCPCommandDescriptors()}
	var ready, done sync.WaitGroup
	ready.Add(1)
	for range 16 {
		done.Go(func() {
			ready.Wait()
			for range 5 {
				descriptors := adapter.MCPCommandDescriptors()
				if len(descriptors) > 0 {
					descriptors[0].Name = "modified-copy"
					if len(descriptors[0].Flags) > 0 {
						descriptors[0].Flags[0] = "modified-copy"
					}
				}
			}
		})
	}
	ready.Done()
	done.Wait()
	for _, descriptor := range adapter.MCPCommandDescriptors() {
		assertions.NotEqual("modified-copy", descriptor.Name)
		assertions.NotContains(descriptor.Flags, "modified-copy")
	}
}
