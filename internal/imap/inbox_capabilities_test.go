package imap

import (
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxIMAPCapabilitiesReportNativeLimitationsWithoutWriting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	client, id := newKeywordTestClient(t, []imapapi.Flag{imapapi.FlagSeen, "Todo"}, false, false)
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	before, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	requirements.NoError(err)
	caps, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
	requirements.NoError(err)
	assertions.Equal(source, caps.Source)
	assertions.Equal("mailboxes", caps.LocationModel)
	assertions.False(caps.ConditionalWrite)
	assertions.False(caps.ObservedAt.IsZero())
	statuses := map[inboxcontrol.Operation]inboxcontrol.CapabilityStatus{}
	for _, entry := range caps.Operations {
		statuses[entry.Operation] = entry.Status
	}
	assertions.Equal(inboxcontrol.CapabilitySupported, statuses[inboxcontrol.OpSetRead])
	assertions.Equal(inboxcontrol.CapabilitySupported, statuses[inboxcontrol.OpTags])
	assertions.Equal(inboxcontrol.CapabilitySupported, statuses[inboxcontrol.OpCreateFolder])
	assertions.Equal(inboxcontrol.CapabilityUnsupported, statuses[inboxcontrol.OpMove])
	assertions.Equal(inboxcontrol.CapabilityUnsupported, statuses[inboxcontrol.OpUnarchive])
	assertions.Equal(inboxcontrol.CapabilityUnsupported, statuses[inboxcontrol.OpArchive])
	after, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	requirements.NoError(err)
	assertions.ElementsMatch(before.Flags, after.Flags)
	assertions.Equal(*before.Read, *after.Read)
	foreign := source
	foreign.SourceID++
	_, err = provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &foreign})
	assertions.ErrorIs(err, inboxcontrol.ErrDenied)
}
