package imap

import (
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func imapCapability(t *testing.T, capabilities *inboxcontrol.Capabilities, operation inboxcontrol.Operation) inboxcontrol.CapabilityStatus {
	t.Helper()
	for _, capability := range capabilities.Operations {
		if capability.Operation == operation {
			return capability.Status
		}
	}
	require.FailNow(t, "missing capability", "%s", operation)
	return ""
}

func TestInboxIMAPCapabilitiesReadOnlyCatalogEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var stores atomic.Int64
	modes := make(chan bool, 8)
	client, id := newKeywordTestClientFor(t, keywordTestSession{available: []imapapi.Flag{imapapi.FlagSeen, "Todo"}, readOnlyPermanent: []imapapi.Flag{}, stores: &stores, selectModes: modes})
	<-modes
	source, _ := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	capabilities, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
	requirements.NoError(err)
	requirements.NotNil(capabilities)
	assertions.Equal(source, capabilities.Source)
	assertions.Equal("mailboxes", capabilities.LocationModel)
	assertions.False(capabilities.ConditionalWrite)
	assertions.False(capabilities.ObservedAt.IsZero())
	assertions.Equal(inboxcontrol.CapabilitySupported, imapCapability(t, capabilities, inboxcontrol.OpTags), "empty EXAMINE PERMANENTFLAGS cannot veto the keyword catalog")
	assertions.Equal(inboxcontrol.CapabilitySupported, imapCapability(t, capabilities, inboxcontrol.OpSetRead))
	assertions.Zero(stores.Load())
	assertions.Empty(client.selectedMailbox)
	requirements.NotEmpty(modes)
	for len(modes) > 0 {
		assertions.True(<-modes)
	}
	foreign := source
	foreign.AccountID = "other@example.test"
	_, err = provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &foreign})
	assertions.ErrorIs(err, inboxcontrol.ErrDenied)
}

func TestInboxIMAPCapabilitiesMoveAndArchiveEvidence(t *testing.T) {
	for _, tc := range []struct {
		fault, configured string
		move, archive     inboxcontrol.CapabilityStatus
	}{
		{fault: "native", move: inboxcontrol.CapabilitySupported, archive: inboxcontrol.CapabilityUnavailable},
		{fault: "no MOVE", configured: "Archive", move: inboxcontrol.CapabilityUnsupported, archive: inboxcontrol.CapabilityUnsupported},
		{fault: "no UIDPLUS", configured: "Archive", move: inboxcontrol.CapabilityUnsupported, archive: inboxcontrol.CapabilityUnsupported},
		{fault: "special", move: inboxcontrol.CapabilitySupported, archive: inboxcontrol.CapabilitySupported},
		{fault: "special ambiguous", move: inboxcontrol.CapabilitySupported, archive: inboxcontrol.CapabilityUnavailable},
		{fault: "special ambiguous", configured: "Archive", move: inboxcontrol.CapabilitySupported, archive: inboxcontrol.CapabilitySupported},
		{fault: "native", configured: "INBOX", move: inboxcontrol.CapabilitySupported, archive: inboxcontrol.CapabilityUnavailable},
	} {
		t.Run(tc.fault+"/"+tc.configured, func(t *testing.T) {
			assertions := assert.New(t)
			f := newInboxNativeFixture(t, tc.fault)
			f.provider.client.config.ArchiveMailbox = tc.configured
			capabilities, err := f.provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &f.source})
			require.NoError(t, err)
			assertions.Equal(tc.move, imapCapability(t, capabilities, inboxcontrol.OpMove))
			assertions.Equal(tc.archive, imapCapability(t, capabilities, inboxcontrol.OpArchive))
			assertions.Zero(f.moves.Load())
			assertions.Zero(f.copies.Load())
			assertions.Zero(f.expunges.Load())
			assertions.Zero(f.creates.Load())
			assertions.Empty(f.provider.client.selectedMailbox)
		})
	}
}

func TestInboxIMAPCapabilitiesMissingFlagCatalogIsUnavailable(t *testing.T) {
	assertions := assert.New(t)
	var stores atomic.Int64
	client, id := newKeywordTestClientFor(t, keywordTestSession{available: []imapapi.Flag{}, readOnlyPermanent: []imapapi.Flag{}, stores: &stores})
	source, _ := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	capabilities, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
	require.NoError(t, err)
	for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpTags, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
		assertions.Equal(inboxcontrol.CapabilityUnavailable, imapCapability(t, capabilities, operation))
	}
	assertions.Zero(stores.Load())
	assertions.Empty(client.selectedMailbox)
}
