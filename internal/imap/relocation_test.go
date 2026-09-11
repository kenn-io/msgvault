package imap

import (
	"context"
	"errors"
	"strconv"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
)

func TestRelocationPlanUsesCurrentMembershipEpochAndExactTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := gmail.MessageRelocationTarget{InternalID: 12, SourceID: 3, SourceMessageID: "Drafts|1", RFC822MessageID: "<same@example.test>"}
	c := &Client{config: &Config{},
		labelMapComplete: true, allMailFolder: "Everything",
		priorFolderStates: map[string]FolderState{
			"Drafts":  {UIDValidity: 77, KnownUIDs: []uint32{1, 2}},
			"Reset":   {UIDValidity: 50, KnownUIDs: []uint32{7}},
			"Deleted": {UIDValidity: 40, KnownUIDs: []uint32{9}},
		},
		observedMailboxDeltas: []MailboxDelta{
			{Mailbox: "Drafts", State: FolderState{UIDValidity: 77, KnownUIDs: []uint32{2}}},
			{Mailbox: "Reset", State: FolderState{UIDValidity: 51, KnownUIDs: []uint32{7}}},
			{Mailbox: "Archive", State: FolderState{UIDValidity: 88, KnownUIDs: []uint32{1}}},
			{Mailbox: "Everything", State: FolderState{UIDValidity: 99, KnownUIDs: []uint32{4, 8}}},
		},
	}
	c.relocationCandidateLoader = func(_ context.Context, lost []string) ([]RelocationCandidate, error) {
		assert.Equal([]string{"Deleted|9", "Drafts|1", "Reset|7"}, lost)
		return []RelocationCandidate{
			{Target: target, Mailbox: "Drafts", UIDValidity: 77, UID: 1},
			{Target: target, Mailbox: "Everything", UIDValidity: 98, UID: 1},
			{Target: target, Mailbox: "Archive", UIDValidity: 88, UID: 1},
			{Target: target, Mailbox: "Everything", UIDValidity: 99, UID: 8},
			{Target: target, Mailbox: "Everything", UIDValidity: 99, UID: 4},
		}, nil
	}
	err := c.finalizeMessageListLocked(t.Context(), []string{"Drafts", "Reset", "Archive", "Everything"}, []gmail.MessageID{{ID: "Archive|1"}, {ID: "Everything|4"}})
	require.NoError(err)
	assert.Equal([]gmail.MessageID{{ID: "Everything|4"}, {ID: "Archive|1"}}, c.messageListCache)
	selected, ok := c.MessageRelocationTarget("Everything|4")
	require.True(ok)
	target.RFC822MessageID = "same@example.test"
	target.NewSourceMessageID = "Everything|4"
	assert.Equal(target, selected)
}

func TestRelocationPlanRejectsStaleAndUnknownIdentities(t *testing.T) {
	for _, candidate := range []RelocationCandidate{
		{Target: gmail.MessageRelocationTarget{InternalID: 1, SourceID: 1, SourceMessageID: "Drafts|1", RFC822MessageID: "same@example.test"}, Mailbox: "Sent", UIDValidity: 87, UID: 1},
		{Target: gmail.MessageRelocationTarget{InternalID: 1, SourceID: 1, SourceMessageID: "Drafts|1"}, Mailbox: "Sent", UIDValidity: 88, UID: 1},
	} {
		c := &Client{config: &Config{}, labelMapComplete: true,
			priorFolderStates:     map[string]FolderState{"Drafts": {UIDValidity: 77, KnownUIDs: []uint32{1}}},
			observedMailboxDeltas: []MailboxDelta{{Mailbox: "Sent", State: FolderState{UIDValidity: 88, KnownUIDs: []uint32{1}}}},
			relocationCandidateLoader: func(context.Context, []string) ([]RelocationCandidate, error) {
				return []RelocationCandidate{candidate}, nil
			},
		}
		require.NoError(t, c.finalizeMessageListLocked(t.Context(), []string{"Sent"}, nil))
		assert.Empty(t, c.messageListCache)
		assert.Empty(t, c.relocationTargets)
	}
}

func TestRelocationLoaderFailureClearsListingAndAllowsRetry(t *testing.T) {
	assert := assert.New(t)
	c := &Client{config: &Config{}, labelMapComplete: true,
		priorFolderStates:     map[string]FolderState{"Drafts": {UIDValidity: 77, KnownUIDs: []uint32{1}}},
		observedMailboxDeltas: []MailboxDelta{},
		messageListCache:      []gmail.MessageID{{ID: "stale|1"}},
		relocationCandidateLoader: func(context.Context, []string) ([]RelocationCandidate, error) {
			return nil, errors.New("database unavailable")
		},
	}
	require.ErrorContains(t, c.finalizeMessageListLocked(t.Context(), nil, nil), "database unavailable")
	assert.Nil(c.messageListCache)
	assert.Nil(c.relocationTargets)
	assert.Nil(c.observedMailboxDeltas)
	assert.False(c.labelMapComplete)
}

func TestRelocationPlanRequiresAuthoritativeCoverage(t *testing.T) {
	for _, mode := range []string{"incomplete labels", "missing mailbox delta", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			c := &Client{config: &Config{}, labelMapComplete: true,
				priorFolderStates:     map[string]FolderState{"Drafts": {UIDValidity: 77, KnownUIDs: []uint32{1}}},
				observedMailboxDeltas: []MailboxDelta{{Mailbox: "Sent", State: FolderState{UIDValidity: 88, KnownUIDs: []uint32{1}}}},
				relocationCandidateLoader: func(context.Context, []string) ([]RelocationCandidate, error) {
					return nil, errors.New("must not infer losses")
				},
			}
			mailboxes := []string{"Sent"}
			switch mode {
			case "incomplete labels":
				c.labelMapComplete = false
			case "missing mailbox delta":
				mailboxes = append(mailboxes, "Drafts")
			case "filtered":
				c.folderFilterInclude = []string{"Sent"}
			}
			require.NoError(t, c.finalizeMessageListLocked(t.Context(), mailboxes, []gmail.MessageID{{ID: "Sent|1"}}))
			assert.Empty(t, c.relocationTargets)
		})
	}
}

func TestRelocationRawFetchRejectsEpochChangeAndBypassesDedup(t *testing.T) {
	for _, epoch := range []uint32{88, 99} {
		t.Run(strconv.FormatUint(uint64(epoch), 10), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			c := &Client{
				selectedUIDValidity:  epoch,
				observedFolderStates: map[string]FolderState{"Sent": {UIDValidity: 88, KnownUIDs: []uint32{1}}},
				relocationTargets:    map[string]gmail.MessageRelocationTarget{"Sent|1": {InternalID: 12, SourceID: 3, SourceMessageID: "Drafts|1", RFC822MessageID: "same@example.test", NewSourceMessageID: "Sent|1"}},
				seenRFC822IDs:        map[string]bool{"same@example.test": true},
				activeSourceAliases:  map[string]string{"Sent|1": "Drafts|1"},
			}
			raw := []byte("Message-ID: <same@example.test>\r\n\r\nfinalword\r\n")
			results := newRawBatchResults([]string{"Sent|1"})
			c.applyFetchResults(results, map[imapapi.UID]int{1: 0}, "Sent", []batchFetchItem{{idx: 0, uid: 1}}, []*imapclient.FetchMessageBuffer{{
				UID: 1, BodySection: []imapclient.FetchBodySectionBuffer{{Bytes: raw}},
			}})
			if epoch == 99 {
				require.Error(results[0].Err)
				require.NotErrorIs(results[0].Err, gmail.ErrMessageGone)
				assert.Nil(results[0].Message)
				assert.Empty(c.ObservedMemberships())
			} else {
				require.NoError(results[0].Err)
				require.NotNil(results[0].Message)
				assert.Equal(raw, results[0].Message.Raw)
				require.Len(c.ObservedMemberships(), 1)
				assert.Equal("Sent|1", c.ObservedMemberships()[0].CanonicalSourceMessageID)
			}
		})
	}
}
