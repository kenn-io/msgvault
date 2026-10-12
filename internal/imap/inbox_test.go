package imap

import (
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func inboxIMAPBinding(c *Client, id DraftReceipt) (inboxcontrol.SourceIdentity, inboxcontrol.Target) {
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "imap", SourceIdentifier: c.config.Identifier(), AccountID: c.config.Username}
	return source, inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: 1, ProviderID: "synthetic-message", Mailbox: id.Mailbox, UIDValidity: id.UIDValidity, UID: id.UID}
}

func TestInboxIMAPSingleFlagActions(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   inboxcontrol.Operation
		tags *emailtags.MessageTagChange
		seen bool
		want []string
	}{
		{name: "read", op: inboxcontrol.OpSetRead, want: []string{`\Flagged`, `\Seen`, "old", "unrelated"}},
		{name: "unread", op: inboxcontrol.OpSetUnread, seen: true, want: []string{`\Flagged`, "old", "unrelated"}},
		{name: "add", op: inboxcontrol.OpTags, tags: &emailtags.MessageTagChange{Add: []string{"Next"}}, seen: true, want: []string{`\Flagged`, `\Seen`, "next", "old", "unrelated"}},
		{name: "remove", op: inboxcontrol.OpTags, tags: &emailtags.MessageTagChange{Remove: []string{"old"}}, seen: true, want: []string{`\Flagged`, `\Seen`, "unrelated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var stores atomic.Int64
			modes := make(chan bool, 16)
			client, id := newKeywordTestClientFor(t, keywordTestSession{available: []imapapi.Flag{"Old", "Next", "Unrelated"}, permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Old", "Next", "Unrelated"}, readOnlyPermanent: []imapapi.Flag{}, stores: &stores, selectModes: modes})
			<-modes
			flags := []imapapi.Flag{imapapi.FlagFlagged, "Old", "Unrelated"}
			if tc.seen {
				flags = append(flags, imapapi.FlagSeen)
			}
			requirements.NoError(client.withConn(t.Context(), func(conn *imapclient.Client) error {
				_, err := conn.Store(imapapi.UIDSetNum(1), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: flags}, nil).Collect()
				return err
			}))
			stores.Store(0)
			source, target := inboxIMAPBinding(client, id)
			provider := NewInboxProvider(client, source)
			request := inboxcontrol.Request{Operation: tc.op, Target: &target, Tags: tc.tags, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			requirements.NotNil(before.Read)
			requirements.NotNil(before.Inbox)
			assertions.True(*before.Inbox)
			assertions.True(<-modes, "observation uses EXAMINE")
			assertions.Empty(client.selectedMailbox, "EXAMINE cannot remain cached as writable")
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			assertions.Zero(stores.Load(), "preview performs no STORE")
			for len(modes) > 0 {
				assertions.True(<-modes, "preview only EXAMINEs")
			}
			request.DryRun = false
			_, err = provider.Dispatch(t.Context(), request, before)
			requirements.NoError(err)
			assertions.Equal(int64(1), stores.Load())
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			requirements.NoError(provider.Verify(request, before, projected, after))
			assertions.ElementsMatch(tc.want, after.Flags)
			assertions.Equal(int64(1), stores.Load(), "readback performs no additional write")
		})
	}
}

func TestInboxIMAPObservationBinding(t *testing.T) {
	client, id := newKeywordTestClientFor(t, keywordTestSession{})
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	for _, field := range []string{"source", "account", "epoch", "uid"} {
		t.Run(field, func(t *testing.T) {
			changed := target
			switch field {
			case "source":
				changed.SourceID++
			case "account":
				changed.AccountID = "other@example.test"
			case "epoch":
				changed.UIDValidity++
			case "uid":
				changed.UID = 99
			}
			_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &changed})
			require.Error(t, err)
		})
	}
}

func TestInboxIMAPPermanentFlagsCheckedAtDispatch(t *testing.T) {
	requirements := require.New(t)
	var stores atomic.Int64
	client, id := newKeywordTestClientFor(t, keywordTestSession{permanent: []imapapi.Flag{imapapi.FlagFlagged}, stores: &stores})
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	_, err = provider.Preview(t.Context(), request, before)
	requirements.NoError(err, "EXAMINE permanent flags cannot prove writable restrictions")
	request.DryRun = false
	_, err = provider.Dispatch(t.Context(), request, before)
	requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
	assert.Zero(t, stores.Load())
}

func TestInboxIMAPFoldersNativeEpoch(t *testing.T) {
	client, id := newKeywordTestClientFor(t, keywordTestSession{})
	source, _ := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	folders, err := provider.Folders(t.Context(), source)
	require.NoError(t, err)
	assert.Equal(t, []inboxcontrol.Folder{{ID: "INBOX", Name: "INBOX", UIDValidity: id.UIDValidity}}, folders)
	foreign := source
	foreign.AccountID = "other@example.test"
	_, err = provider.Folders(t.Context(), foreign)
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
}

func TestInboxIMAPVerifyRequiresCompleteConsistentReadback(t *testing.T) {
	client, id := newKeywordTestClientFor(t, keywordTestSession{})
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetUnread, Target: &target, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	require.NoError(t, err)
	projected, err := provider.Preview(t.Context(), request, before)
	require.NoError(t, err)
	for _, field := range []string{"FLAGS", "tags", "Seen marker", "location", "UID"} {
		t.Run(field, func(t *testing.T) {
			after := before
			switch field {
			case "FLAGS":
				after.Flags = nil
			case "tags":
				after.Tags = []string{"Invented"}
			case "Seen marker":
				after.Flags = []string{`\Seen`}
			case "location":
				after.Location = "Archive"
			case "UID":
				after.Target.UID++
			}
			require.ErrorIs(t, provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
		})
	}
}

func TestInboxIMAPVerifyRejectsMissingPlanEvidence(t *testing.T) {
	client, id := newKeywordTestClientFor(t, keywordTestSession{})
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetUnread, Target: &target, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	require.NoError(t, err)
	projected, err := provider.Preview(t.Context(), request, before)
	require.NoError(t, err)
	for _, missing := range []string{"before FLAGS", "projected FLAGS"} {
		t.Run(missing, func(t *testing.T) {
			original, plan := before, projected
			if missing == "before FLAGS" {
				original.Flags = nil
			} else {
				plan.Flags = nil
			}
			require.ErrorIs(t, provider.Verify(request, original, plan, before), inboxcontrol.ErrOutcomeUnknown)
		})
	}
}
