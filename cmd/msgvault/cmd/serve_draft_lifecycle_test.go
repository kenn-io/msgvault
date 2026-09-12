package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type draftLifecycleFixture struct {
	draftReplyFixture
	draftID int64
}

func newDraftLifecycleFixture(t *testing.T) draftLifecycleFixture {
	requirements := require.New(t)

	t.Helper()
	f := newDraftReplyFixture(t)
	events, err := f.run(t, f.grantedAdapter(), "--body", "Initial draft body", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result map[string]any
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	messageID, ok := result["message_id"].(float64)
	requirements.True(ok)
	requirements.Positive(messageID)
	return draftLifecycleFixture{draftReplyFixture: f, draftID: int64(messageID)}
}

func (f draftLifecycleFixture) runGet(t *testing.T, args ...string) ([]api.CLIRunEvent, error) {
	t.Helper()
	adapter := f.grantedAdapter()
	var events []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func decodeDraftLifecycleEvent(t *testing.T, event api.CLIRunEvent) map[string]any {
	requirements := require.New(t)

	t.Helper()
	var result map[string]any
	requirements.NoError(json.Unmarshal([]byte(event.Data), &result))
	return result
}

func TestDraftGetReportsLocalAndRemoteState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	const bodyText = "  Initial draft body\nwith a second line  "
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE message_bodies SET body_text = ? WHERE message_id = ?
	`), bodyText, f.draftID)
	requirements.NoError(err)
	events, err := f.runGet(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	result := decodeDraftLifecycleEvent(t, events[0])
	assertions.Equal("active", result["lifecycle"])
	assertions.Equal("present", result["provider_status"])
	assertions.Equal(float64(f.draftID), result["draft_id"])
	assertions.Equal("Re: Question", result["subject"])
	assertions.Equal(bodyText, result["body_text"])
}

func TestDraftGetTextIncludesHeadersAndBody(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	events, err := f.runGet(t, "draft-get", strconv.FormatInt(f.draftID, 10))
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, "subject: Re: Question")
	assertions.Contains(events[0].Data, "from: alice@example.com")
	assertions.Contains(events[0].Data, "body:\nInitial draft body")
}

func TestDraftGetReportsAbsentRemoteState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	remoteClient := imaplib.NewClient(f.config, testutil.IMAPTestPassword)
	defer func() { _ = remoteClient.Close() }()
	requirements.NoError(remoteClient.DeleteMessage(t.Context(), fmt.Sprintf("Drafts|%d", 1)))

	events, err := f.runGet(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("absent", decodeDraftLifecycleEvent(t, events[0])["provider_status"])
}

func TestDraftGetReportsUnknownProviderOnUIDValidityChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_message_memberships SET uidvalidity = uidvalidity + 1
		WHERE source_id = ? AND message_id = ?
	`), f.source.ID, f.draftID)
	requirements.NoError(err)

	events, err := f.runGet(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("unknown", decodeDraftLifecycleEvent(t, events[0])["provider_status"])
}

func TestDraftGetReportsNotCheckedWithoutMatchingGrant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	opened := false
	adapter := &storeAPIAdapter{
		store: f.store,
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			opened = true
			return nil, errors.New("draft-get must not open IMAP without a grant")
		},
	}
	var events []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-get", strconv.FormatInt(f.draftID, 10), "--json"},
	}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("not_checked", decodeDraftLifecycleEvent(t, events[0])["provider_status"])
	assertions.False(opened)
}

func TestDraftGetReportsNotCheckedForWrongGrantMailbox(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	opened := false
	adapter := &storeAPIAdapter{
		store:       f.store,
		draftPolicy: []config.IMAPDraftSource{{SourceID: f.source.ID, Enabled: true, Mailbox: "Other"}},
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			opened = true
			return nil, errors.New("draft-get must not open IMAP for a mailbox mismatch")
		},
	}
	var events []api.CLIRunEvent
	err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-get", strconv.FormatInt(f.draftID, 10), "--json"},
	}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("not_checked", decodeDraftLifecycleEvent(t, events[0])["provider_status"])
	assertions.False(opened)
}

func TestDraftGetRejectsInvalidSourceBeforeOpeningClient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	cases := []struct {
		name   string
		config string
	}{
		{name: "malformed config", config: "{"},
		{name: "identifier mismatch", config: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDraftLifecycleFixture(t)
			configJSON := tc.config
			if configJSON == "" {
				configJSON = `{"host":"other.example.test","port":143,"username":"different@example.test"}`
			}
			requirements.NoError(f.store.UpdateSourceSyncConfig(f.source.ID, configJSON))
			opened := false
			adapter := f.grantedAdapter()
			adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
				opened = true
				return nil, errors.New("invalid source must not open IMAP")
			}
			err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
				Args: []string{"draft-get", strconv.FormatInt(f.draftID, 10)},
			}, nil)
			requirements.Error(err)
			assertions.Equal("invalid_source", err.Error())
			assertions.False(opened)
		})
	}
}

func TestDraftGetMissingOwnershipDoesNotOpenIMAP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftReplyFixture(t)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO imap_message_memberships
			(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`), f.source.ID, "Drafts", 101, 9, f.parentID, `["\\Draft"]`)
	requirements.NoError(err)
	opened := false
	adapter := &storeAPIAdapter{
		store:       f.store,
		draftPolicy: []config.IMAPDraftSource{{SourceID: f.source.ID, Enabled: true, Mailbox: "Drafts"}},
		draftClientFactory: func(context.Context, *store.Source) (*imaplib.Client, error) {
			opened = true
			return nil, errors.New("ordinary messages must not reach IMAP")
		},
	}
	err = adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-get", strconv.FormatInt(f.parentID, 10)},
	}, nil)
	requirements.Error(err)
	assertions.Equal("draft_not_found", err.Error())
	assertions.False(opened)
}

func TestDraftGetFollowsCurrentMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	var messageID int64
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT current_message_id FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&messageID))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		DELETE FROM imap_message_memberships
		WHERE source_id = ? AND message_id = ?
	`), f.source.ID, messageID)
	requirements.NoError(err)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO imap_message_memberships
			(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`), f.source.ID, "Archive", 202, 77, messageID, `[]`)
	requirements.NoError(err)

	events, err := f.runGet(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	requirements.NoError(err)
	result := decodeDraftLifecycleEvent(t, events[0])
	assertions.Equal("Archive", result["mailbox"])
	assertions.Equal(float64(77), result["uid"])
	assertions.Equal(float64(202), result["uidvalidity"])
}

func TestDraftGetReturnsDiscardedLocalStateWithoutOpeningClient(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE imap_drafts SET lifecycle = 'discarded' WHERE draft_id = ?
	`), f.draftID)
	requirements.NoError(err)
	opened := false
	adapter := f.grantedAdapter()
	adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		opened = true
		return nil, errors.New("discarded draft must not open IMAP")
	}
	var events []api.CLIRunEvent
	err = adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
		Args: []string{"draft-get", strconv.FormatInt(f.draftID, 10), "--json"},
	}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Equal("discarded", decodeDraftLifecycleEvent(t, events[0])["lifecycle"])
	assertions.False(opened)
}

func TestParseDraftLifecycleArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	intent, err := parseDraftLifecycleArgs([]string{
		"draft-get", "42", "--json", "--log-level", "debug", "--verbose", "--log-sql", "--log-sql-slow-ms=10",
	})
	requirements.NoError(err)
	assertions.Equal("draft-get", intent.Command)
	assertions.Equal(int64(42), intent.DraftID)
	assertions.True(intent.JSON)

	for _, args := range [][]string{
		{"draft-edit", "42"},
		{"draft-delete", "42"},
		{"draft-get", "42", "--revision=1"},
		{"draft-get", "42", "--body=text"},
		{"draft-get", "42", "--unknown"},
		{"draft-get", "0"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := parseDraftLifecycleArgs(args)
			requirements.Error(err)
			assertions.Equal("invalid_args", err.Error())
		})
	}
}

func TestParseDraftLifecycleArgsAcceptsInheritedBooleanValues(t *testing.T) {
	requirements := require.New(t)

	intent, err := parseDraftLifecycleArgs([]string{
		"draft-get", "42", "--json=false", "--verbose=false", "--log-sql=false",
	})
	requirements.NoError(err)
	requirements.False(intent.JSON)
}

func TestDraftLifecycleRejectsEnvironmentAndWorkingDirectory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	adapter := &storeAPIAdapter{}
	for _, request := range []api.CLIRunRequest{
		{Args: []string{"draft-get", "1"}, Env: map[string]string{"EDITOR": "vim"}},
		{Args: []string{"draft-get", "1"}, Cwd: "C:\\Temp"},
	} {
		err := adapter.runCLIDraftLifecycle(context.Background(), request, nil)
		requirements.Error(err)
		assertions.Equal("invalid_args", err.Error())
	}
}

func TestDraftLifecycleCommandOnlyRegistersGet(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	command := newDraftGetCommand()
	assertions.Equal("draft-get <draft-id>", command.Use)
	requirements.Error(command.Args(command, nil))
	requirements.NoError(command.Args(command, []string{"1"}))

	_, err := parseDraftLifecycleArgs([]string{"draft-delete", "1", "--revision=1"})
	requirements.Error(err)
}

func TestDraftGetReadDoesNotChangeOwnershipRow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := newDraftLifecycleFixture(t)
	var before string
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT updated_at FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&before))
	_, err := f.runGet(t, "draft-get", strconv.FormatInt(f.draftID, 10), "--json")
	requirements.NoError(err)
	var after string
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT updated_at FROM imap_drafts WHERE draft_id = ?
	`), f.draftID).Scan(&after))
	assertions.Equal(before, after)
}
