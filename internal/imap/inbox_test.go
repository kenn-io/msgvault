package imap

import (
	"context"
	"fmt"
	"slices"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func inboxIMAPBinding(c *Client, id KeywordIdentity) (inboxcontrol.SourceIdentity, inboxcontrol.Target) {
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "imap", SourceIdentifier: c.config.Identifier(), AccountID: c.config.Username}
	target := inboxcontrol.Target{SourceID: source.SourceID, SourceType: "imap", SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: 1, ProviderID: "synthetic-archive-message", Mailbox: id.Mailbox, UIDValidity: id.UIDValidity, UID: id.UID}
	return source, target
}

func TestInboxIMAPSeenDeltaPreservesKeywordsAndOtherFlags(t *testing.T) {
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
		t.Run(string(op), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client, id := newKeywordTestClient(t, []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, imapapi.FlagWildcard}, false, false)
			err := client.withConn(t.Context(), func(conn *imapclient.Client) error {
				flags := []imapapi.Flag{imapapi.FlagFlagged, "Todo", "Watch", "Unrelated"}
				if op == inboxcontrol.OpSetUnread {
					flags = append(flags, imapapi.FlagSeen)
				}
				_, err := conn.Store(imapapi.UIDSetNum(imapapi.UID(id.UID)), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: flags}, nil).Collect()
				if err != nil {
					return fmt.Errorf("seed native IMAP flags: %w", err)
				}
				return nil
			})
			requirements.NoError(err)
			source, target := inboxIMAPBinding(client, id)
			provider := NewInboxProvider(client, source)
			request := inboxcontrol.Request{Operation: op, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			requirements.NotNil(before.Read)
			requirements.NotNil(before.Inbox)
			assertions.True(*before.Inbox)
			assertions.Equal(op == inboxcontrol.OpSetUnread, *before.Read)
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			previewRead, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			assertions.Equal(*before.Read, *previewRead.Read)
			_, err = provider.Dispatch(t.Context(), request, before)
			requirements.NoError(err)
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			requirements.NoError(provider.Verify(request, before, projected, after))
			assertions.Equal(op == inboxcontrol.OpSetRead, *after.Read)
			assertions.True(*after.Inbox)
			requirements.Len(before.Tags, 3)
			assertions.ElementsMatch(before.Tags, after.Tags)
			assertions.Contains(after.Flags, string(imapapi.FlagFlagged))
		})
	}
}

func TestInboxIMAPObservationRejectsStaleOrForeignIdentity(t *testing.T) {
	client, id := newKeywordTestClient(t, nil, false, false)
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	for _, invalid := range []string{"epoch", "uid", "account", "source"} {
		t.Run(invalid, func(t *testing.T) {
			changed := target
			switch invalid {
			case "epoch":
				changed.UIDValidity++
			case "uid":
				changed.UID = 99
			case "account":
				changed.AccountID = "other@example.com"
			case "source":
				changed.SourceID++
			}
			_, err := provider.Observe(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &changed})
			assert.Error(t, err)
		})
	}
}

func TestInboxIMAPRejectsUnknownFlagsAndNonpersistentSeen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []imapapi.Flag
		omit  bool
	}{
		{name: "flags absent", omit: true},
		{name: "Seen not permanent", flags: []imapapi.Flag{imapapi.FlagFlagged}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client, id := newKeywordTestClient(t, tc.flags, false, tc.omit)
			source, target := inboxIMAPBinding(client, id)
			provider := NewInboxProvider(client, source)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			if tc.omit {
				assertions.Error(err)
				return
			}
			requirements.NoError(err)
			_, err = provider.Preview(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			assertions.False(*after.Read)
		})
	}
}

func TestInboxIMAPFoldersReturnLiveMailboxEpoch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	client, id := newKeywordTestClient(t, nil, false, false)
	source, _ := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	folders, err := provider.Folders(t.Context(), source)
	requirements.NoError(err)
	requirements.Len(folders, 1)
	assertions.Equal(inboxcontrol.Folder{ID: "INBOX", Name: "INBOX", UIDValidity: id.UIDValidity}, folders[0])
	foreign := source
	foreign.AccountID = "other@example.com"
	_, err = provider.Folders(t.Context(), foreign)
	assertions.ErrorIs(err, inboxcontrol.ErrDenied)
}

func TestInboxIMAPKeywordDeltaPreservesSeenAndExistingFlags(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	client, id := newKeywordTestClient(t, []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Old", "Next", "Unrelated"}, false, false)
	requirements.NoError(client.withConn(t.Context(), func(conn *imapclient.Client) error {
		_, err := conn.Store(imapapi.UIDSetNum(1), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Old", "Unrelated"}}, nil).Collect()
		if err != nil {
			return fmt.Errorf("seed native IMAP flags: %w", err)
		}
		return nil
	}))
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{"Next"}, Remove: []string{"old"}}, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	unchanged, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	assertions.ElementsMatch(before.Flags, unchanged.Flags)
	_, err = provider.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(request, before, projected, after))
	assertions.True(*after.Read)
	assertions.True(*after.Inbox)
	assertions.Contains(after.Flags, string(imapapi.FlagFlagged))
	assertions.ElementsMatch([]string{"next", "unrelated"}, after.Tags)
	// Readback must prove removal as well as additions and unrelated preservation.
	wrong := after
	wrong.Flags = append(slices.Clone(after.Flags), "old")
	wrong.Tags = append(slices.Clone(after.Tags), "old")
	assertions.ErrorIs(provider.Verify(request, before, projected, wrong), inboxcontrol.ErrOutcomeUnknown)
}

func TestInboxIMAPKeywordsRequireExistingPersistentSupport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		permanent []imapapi.Flag
		keyword   string
	}{
		{name: "unsupported persistence", permanent: []imapapi.Flag{imapapi.FlagSeen}, keyword: "Old"},
		{name: "new keyword with wildcard", permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagWildcard}, keyword: "New"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client, id := newKeywordTestClient(t, tc.permanent, false, false)
			source, target := inboxIMAPBinding(client, id)
			provider := NewInboxProvider(client, source)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{tc.keyword}}, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			_, err = provider.Preview(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			_, err = provider.Dispatch(t.Context(), request, before)
			require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			assertions.ElementsMatch(before.Flags, after.Flags)
		})
	}
}

func TestInboxIMAPPartialKeywordWriteStaysUnknown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	client, id := newKeywordTestClient(t, []imapapi.Flag{imapapi.FlagSeen, "Old", "Next"}, true, false)
	requirements.NoError(client.withConn(t.Context(), func(conn *imapclient.Client) error {
		_, err := conn.Store(imapapi.UIDSetNum(1), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: []imapapi.Flag{imapapi.FlagSeen, "Old"}}, nil).Collect()
		if err != nil {
			return fmt.Errorf("seed native IMAP flags: %w", err)
		}
		return nil
	}))
	source, target := inboxIMAPBinding(client, id)
	provider := NewInboxProvider(client, source)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{"Next"}, Remove: []string{"Old"}}, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	_, err = provider.Dispatch(t.Context(), request, before)
	require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	assertions.ElementsMatch([]string{"old", "next"}, after.Tags)
	assertions.True(*after.Read)
	assertions.ErrorIs(provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
}

func TestInboxIMAPSyncObservationPreservesFlagPresence(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted=%v", omitted), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client, _ := newKeywordTestClient(t, nil, false, omitted)
			_, err := client.GetMessageRaw(t.Context(), "INBOX|1")
			requirements.NoError(err)
			observed := client.ObservedMemberships()
			requirements.NotEmpty(observed)
			if omitted {
				assertions.Nil(observed[len(observed)-1].Flags, "missing FLAGS must remain unknown for provider read state")
			} else {
				assertions.NotNil(observed[len(observed)-1].Flags, "empty FLAGS must prove unread")
				assertions.Empty(observed[len(observed)-1].Flags)
			}
		})
	}
}
