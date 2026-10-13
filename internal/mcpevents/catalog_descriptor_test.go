package mcpevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcecatalog"
)

func TestNativeSourceDescriptorsMatchImplementedMessageCatalog(t *testing.T) {
	// Consume the actual registered descriptors and production Events catalog.
	// A descriptor must not claim a live producer absent from that catalog, or
	// conceal a producer that the native Events runtime already advertises.
	for _, descriptor := range sourcecatalog.List() {
		t.Run(descriptor.Type, func(t *testing.T) {
			caps := capabilities(true, []string{descriptor.Type})
			live := false
			for _, capability := range caps {
				if capability.Family == messageFamily {
					live = true
					require.Contains(t, capability.Kinds, "message")
				}
			}
			assert.Equal(t, live, descriptor.LiveMessages)
		})
	}
}
