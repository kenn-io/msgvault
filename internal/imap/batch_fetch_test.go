package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNewRawBatchResultsKeepsInputIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newRawBatchResults([]string{"Archive|10", "Archive|11"})

	require.Len(results, 2)
	assert.Equal("Archive|10", results[0].ID)
	assert.Nil(results[0].Message)
	require.NoError(results[0].Err)
	assert.Equal("Archive|11", results[1].ID)
	assert.Nil(results[1].Message)
	require.NoError(results[1].Err)
}

func TestMarkRawBatchErrorMarksOnlyRequestedItems(t *testing.T) {
	require := require.New(t)
	errFetch := errors.New("fetch failed")
	results := newRawBatchResults([]string{"Archive|10", "Archive|11", "Archive|12"})
	items := []batchFetchItem{
		{idx: 0, uid: imapapi.UID(10)},
		{idx: 2, uid: imapapi.UID(12)},
	}

	markRawBatchError(results, items, errFetch)

	require.ErrorIs(results[0].Err, errFetch)
	require.NoError(results[1].Err)
	require.ErrorIs(results[2].Err, errFetch)
}

func TestRawBatchMessagesDropsPerItemErrorsForLegacyCallers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	msg0 := &gmailapi.RawMessage{ID: "Archive|10", Raw: []byte("raw-10")}
	msg2 := &gmailapi.RawMessage{ID: "Archive|12", Raw: []byte("raw-12")}
	results := []gmailapi.RawMessageBatchResult{
		{ID: "Archive|10", Message: msg0},
		{ID: "Archive|11", Err: errors.New("fetch failed")},
		{ID: "Archive|12", Message: msg2},
	}

	messages := rawBatchMessages(results)

	require.Len(messages, 3)
	assert.Same(msg0, messages[0])
	assert.Nil(messages[1])
	assert.Same(msg2, messages[2])
}

func TestBatchMailboxOrderPutsAllMailFirst(t *testing.T) {
	byMailbox := map[string][]batchFetchItem{
		"Trash":    {{idx: 0, uid: imapapi.UID(1)}},
		"Archive":  {{idx: 1, uid: imapapi.UID(2)}},
		"All Mail": {{idx: 2, uid: imapapi.UID(3)}},
	}

	assert.Equal(t,
		[]string{"Trash", "All Mail", "Archive"},
		batchMailboxOrder(byMailbox, "Trash"))
	assert.Equal(t,
		[]string{"All Mail", "Archive", "Trash"},
		batchMailboxOrder(byMailbox, ""))
}

func TestMessageIDHeaderFetchOptionsAvoidsRawMessageBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	opts := messageIDHeaderFetchOptions()

	assert.True(opts.UID)
	assert.True(opts.Flags)
	assert.False(opts.InternalDate)
	assert.False(opts.RFC822Size)
	require.Len(opts.BodySection, 1)
	assert.True(opts.BodySection[0].Peek)
	assert.Equal(imapapi.PartSpecifierHeader, opts.BodySection[0].Specifier)
	assert.Equal([]string{"Message-ID"}, opts.BodySection[0].HeaderFields)
}

func TestRawBatchFetchRecordsMembershipBeforeDedup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{"All Mail": 0, "Archive": 0},
		map[string][]imapapi.MailboxAttr{"All Mail": {imapapi.MailboxAttrAll}},
	)
	const messageID = "membership-dedup@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "All Mail", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", messageID)
	client := newTestClient(t, addr)
	require.ElementsMatch([]string{"All Mail|1", "Archive|1"}, listAllMessages(t, client))

	client.mu.Lock()
	client.observedMemberships = nil
	var uidSet imapapi.UIDSet
	uidSet.AddNum(1)
	allMailSelectErr := client.selectMailbox("All Mail")
	var allMailStoreErr error
	if allMailSelectErr == nil {
		_, allMailStoreErr = client.conn.Store(uidSet, &imapapi.StoreFlags{
			Op: imapapi.StoreFlagsSet, Flags: []imapapi.Flag{imapapi.FlagSeen},
		}, nil).Collect()
	}
	archiveSelectErr := client.selectMailbox("Archive")
	var archiveStoreErr error
	if archiveSelectErr == nil {
		_, archiveStoreErr = client.conn.Store(uidSet, &imapapi.StoreFlags{
			Op: imapapi.StoreFlagsSet, Flags: []imapapi.Flag{imapapi.FlagFlagged},
		}, nil).Collect()
	}
	client.mu.Unlock()
	require.NoError(allMailSelectErr)
	require.NoError(allMailStoreErr)
	require.NoError(archiveSelectErr)
	require.NoError(archiveStoreErr)
	results, err := client.GetMessagesRawBatchWithErrors(
		context.Background(), []string{"All Mail|1", "Archive|1"})
	require.NoError(err)
	require.Len(results, 2)
	require.NotNil(results[0].Message)
	require.NotNil(results[1].Message)
	assert.NotEmpty(results[0].Message.Raw)
	assert.Empty(results[1].Message.Raw, "the overlapping copy is returned as a duplicate stub")

	observed := client.ObservedMemberships()
	require.Len(observed, 2, "both raw FETCH results must be recorded before deduplication")
	assert.Equal("All Mail", observed[0].Mailbox)
	assert.NotZero(observed[0].UIDValidity)
	assert.Equal(uint32(1), observed[0].UID)
	assert.Equal("All Mail|1", observed[0].SourceMessageID)
	assert.Equal(messageID, observed[0].RFC822MessageID)
	assert.Equal([]string{"\\Seen"}, observed[0].Flags)
	assert.Equal("Archive", observed[1].Mailbox)
	assert.NotZero(observed[1].UIDValidity)
	assert.Equal(uint32(1), observed[1].UID)
	assert.Equal("Archive|1", observed[1].SourceMessageID)
	assert.Equal(messageID, observed[1].RFC822MessageID)
	assert.Equal([]string{"\\Flagged"}, observed[1].Flags)
}

func TestRawBatchFetchDoesNotRequestMalformedEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	raw := []byte("Message-ID: <tencent-914@example.test>\r\nSubject: archived message\r\n\r\nbody")
	addr, fetchCommand := startMalformedEnvelopeIMAPServer(t, raw)
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword)
	t.Cleanup(func() { _ = client.Close() })

	results, err := client.GetMessagesRawBatchWithErrors(
		t.Context(), []string{"INBOX|914"})

	require.NoError(err)
	require.Len(results, 1)
	require.NoError(results[0].Err)
	require.NotNil(results[0].Message)
	assert.Equal(raw, results[0].Message.Raw)
	assert.NotContains(<-fetchCommand, "ENVELOPE")
}

func TestLabelOnlyFetchRecordsCanonicalMembershipFlags(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	const messageID = "label-membership@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	client := newTestClient(t, addr)
	require.Equal([]string{"INBOX|1"}, listAllMessages(t, client))

	client.mu.Lock()
	client.observedMemberships = nil
	selectErr := client.selectMailbox("INBOX")
	var uidSet imapapi.UIDSet
	uidSet.AddNum(1)
	var storeErr error
	if selectErr == nil {
		_, storeErr = client.conn.Store(uidSet, &imapapi.StoreFlags{
			Op:    imapapi.StoreFlagsSet,
			Flags: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "$Forwarded"},
		}, nil).Collect()
	}
	client.mu.Unlock()
	require.NoError(selectErr)
	require.NoError(storeErr)

	results, err := client.GetMessageLabelsBatch(context.Background(), []string{"INBOX|1"})
	require.NoError(err)
	require.Len(results, 1)
	require.NoError(results[0].Err)

	observed := client.ObservedMemberships()
	require.Len(observed, 1)
	assert.Equal(MembershipObservation{
		Mailbox:         "INBOX",
		UIDValidity:     observed[0].UIDValidity,
		UID:             1,
		SourceMessageID: "INBOX|1",
		RFC822MessageID: messageID,
		Flags:           []string{"$Forwarded", "\\Flagged", "\\Seen"},
	}, observed[0])
	assert.NotZero(observed[0].UIDValidity)
}

func TestObservedMembershipsReturnsDefensiveCopy(t *testing.T) {
	client := &Client{observedMemberships: []MembershipObservation{{
		Mailbox: "INBOX", UID: 1, Flags: []string{"\\Seen"},
	}}}

	first := client.ObservedMemberships()
	first[0].Mailbox = "mutated"
	first[0].Flags[0] = "mutated"
	first = append(first, MembershipObservation{Mailbox: "extra"})
	assert.Len(t, first, 2)

	assert.Equal(t, []MembershipObservation{{
		Mailbox: "INBOX", UID: 1, Flags: []string{"\\Seen"},
	}}, client.ObservedMemberships())
}

func TestGetMessageLabelsBatchPreservesPerMessageMissingUID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	client := newTestClient(t, addr)

	results, err := client.GetMessageLabelsBatch(
		context.Background(),
		[]string{"INBOX|1", "INBOX|99"},
	)

	require.NoError(err)
	require.Len(results, 2)
	assert.Equal("INBOX|1", results[0].ID)
	assert.Equal([]string{"INBOX"}, results[0].LabelIDs)
	require.NoError(results[0].Err)
	assert.Equal("INBOX|99", results[1].ID)
	assert.Nil(results[1].LabelIDs)
	require.ErrorIs(results[1].Err, errIMAPFetchResultMissing)
}

func TestLabelOnlyRescanDefersAllMailDedupUntilValidated(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(
		t,
		map[string]int{
			"All Mail": 0,
			"Archive":  0,
		},
		map[string][]imapapi.MailboxAttr{
			"All Mail": {imapapi.MailboxAttrAll},
		},
	)
	const messageID = "overlapping-rescan@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "All Mail", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", messageID)
	client := newTestClient(t, addr)

	require.ElementsMatch(
		[]string{"All Mail|1", "Archive|1"},
		listAllMessages(t, client),
	)

	labelResults, err := client.GetMessageLabelsBatch(
		context.Background(),
		[]string{"All Mail|1"},
	)
	require.NoError(err)
	require.Len(labelResults, 1)
	require.NoError(labelResults[0].Err)
	assert.ElementsMatch(
		[]string{"All Mail", "Archive"},
		labelResults[0].LabelIDs,
	)
	assert.Equal(messageID, labelResults[0].RFC822MessageID)
	assert.NotContains(client.seenRFC822IDs, messageID,
		"label metadata must not become dedup authority before validation")

	require.NoError(client.SeedValidatedMessageDedup(
		"All Mail|1",
		labelResults[0].RFC822MessageID,
	))
	assert.Contains(client.seenRFC822IDs, messageID)

	rawResults, err := client.GetMessagesRawBatchWithErrors(
		context.Background(),
		[]string{"Archive|1"},
	)
	require.NoError(err)
	require.Len(rawResults, 1)
	require.NoError(rawResults[0].Err)
	require.NotNil(rawResults[0].Message)
	assert.Nil(rawResults[0].Message.Raw,
		"the overlapping mailbox copy must remain a dedup stub")
}

func TestSeedValidatedMessageDedupEligibility(t *testing.T) {
	tests := []struct {
		name             string
		messageID        string
		rfc822MessageID  string
		allMailFolder    string
		labelMapComplete bool
		wantSeen         map[string]bool
		wantErr          bool
	}{
		{
			name:            "canonical all mail",
			messageID:       "All Mail|1",
			rfc822MessageID: "canonical@example.com",
			allMailFolder:   "All Mail",
			wantSeen: map[string]bool{
				"existing@example.com":  true,
				"canonical@example.com": true,
			},
		},
		{
			name:             "complete server without all mail",
			messageID:        "Archive|1",
			rfc822MessageID:  "complete@example.com",
			labelMapComplete: true,
			wantSeen: map[string]bool{
				"existing@example.com": true,
				"complete@example.com": true,
			},
		},
		{
			name:            "noncanonical mailbox on all mail server",
			messageID:       "Archive|1",
			rfc822MessageID: "alternate@example.com",
			allMailFolder:   "All Mail",
			wantSeen: map[string]bool{
				"existing@example.com": true,
			},
		},
		{
			name:            "incomplete server without all mail",
			messageID:       "Archive|1",
			rfc822MessageID: "incomplete@example.com",
			wantSeen: map[string]bool{
				"existing@example.com": true,
			},
		},
		{
			name:             "empty rfc822 identity",
			messageID:        "Archive|1",
			labelMapComplete: true,
			wantSeen: map[string]bool{
				"existing@example.com": true,
			},
		},
		{
			name:            "malformed composite id",
			messageID:       "not-a-composite-id",
			rfc822MessageID: "malformed@example.com",
			wantSeen: map[string]bool{
				"existing@example.com": true,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{
				allMailFolder:    tt.allMailFolder,
				labelMapComplete: tt.labelMapComplete,
				seenRFC822IDs: map[string]bool{
					"existing@example.com": true,
				},
			}

			err := c.SeedValidatedMessageDedup(
				tt.messageID,
				tt.rfc822MessageID,
			)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantSeen, c.seenRFC822IDs)
		})
	}
}

func TestIncompleteLabelMapDisablesCrossMailboxDedup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, user := testutil.StartIMAPMemServerWithOneShotSelectError(
		t,
		map[string]int{
			"All Mail": 0,
			"Archive":  0,
		},
		map[string][]imapapi.MailboxAttr{
			"All Mail": {imapapi.MailboxAttrAll},
		},
		"Archive",
	)
	const messageID = "incomplete-membership@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "All Mail", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", messageID)
	client := newTestClient(t, addr)

	ids := listAllMessages(t, client)
	require.ElementsMatch(
		[]string{"All Mail|1", "Archive|1"},
		ids,
	)
	assert.False(client.LabelsSnapshotComplete())

	results, err := client.GetMessagesRawBatchWithErrors(
		context.Background(), ids)
	require.NoError(err)
	require.Len(results, 2)
	for _, result := range results {
		require.NoError(result.Err)
		require.NotNil(result.Message)
		require.NotEmpty(result.Message.Raw,
			"an incomplete membership map must not produce dedup stubs")
		mailbox, _, err := parseCompositeID(result.ID)
		require.NoError(err)
		assert.Contains(result.Message.LabelIDs, mailbox)
	}
}

func TestApplyLabelFetchResultsMarksOnlyMissingUID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newLabelBatchResults([]string{"Archive|10", "Archive|11"})
	uidToIdx := map[imapapi.UID]int{
		imapapi.UID(10): 0,
		imapapi.UID(11): 1,
	}
	chunk := []batchFetchItem{
		{idx: 0, uid: imapapi.UID(10)},
		{idx: 1, uid: imapapi.UID(11)},
	}
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBuffer(
			"message-10",
			[]byte("Message-ID: <message-10@example.com>\r\n\r\n"),
		),
	}

	var c Client
	omitted := c.applyLabelFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	assert.Equal([]string{"Archive"}, results[0].LabelIDs)
	require.NoError(results[0].Err)
	assert.Nil(results[1].LabelIDs)
	// The omission is reported, not acted on. Calling it gone is the caller's
	// decision, and only after the server has been asked a second time.
	assert.Equal([]batchFetchItem{{idx: 1, uid: imapapi.UID(11)}}, omitted)
	require.NoError(results[1].Err)
}

func TestApplyFetchResultsMarksMissingUIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newRawBatchResults([]string{"Archive|10", "Archive|11"})
	uidToIdx := map[imapapi.UID]int{
		imapapi.UID(10): 0,
		imapapi.UID(11): 1,
	}
	chunk := []batchFetchItem{
		{idx: 0, uid: imapapi.UID(10)},
		{idx: 1, uid: imapapi.UID(11)},
	}
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBuffer("message-10", []byte("raw-10")),
	}

	var c Client
	omitted := c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(results[0].Message)
	assert.Equal("Archive|10", results[0].Message.ID)
	assert.Equal([]byte("raw-10"), results[0].Message.Raw)
	require.NoError(results[0].Err)
	assert.Nil(results[1].Message)
	assert.Equal([]batchFetchItem{{idx: 1, uid: imapapi.UID(11)}}, omitted)
	require.NoError(results[1].Err)
}

func TestApplyFetchResultsMarksMissingRawBody(t *testing.T) {
	tests := []struct {
		name string
		msg  *imapclient.FetchMessageBuffer
	}{
		{
			name: "no body section",
			msg: &imapclient.FetchMessageBuffer{
				UID:      imapapi.UID(10),
				Envelope: &imapapi.Envelope{MessageID: "message-10"},
			},
		},
		{
			name: "empty body",
			msg:  fetchMessageBuffer("message-10", nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := newRawBatchResults([]string{"Archive|10"})
			uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
			chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}

			var c Client
			c.applyFetchResults(results, uidToIdx, "Archive", chunk, []*imapclient.FetchMessageBuffer{tt.msg})

			assert.Nil(t, results[0].Message)
			require.ErrorIs(t, results[0].Err, errIMAPRawBodyMissing)
		})
	}
}

func TestApplyFetchResultsPreservesDedupStub(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBuffer(
			"duplicate@example.com",
			[]byte("Message-ID: <duplicate@example.com>\r\n\r\nbody"),
		),
	}
	c := Client{
		seenRFC822IDs: map[string]bool{"duplicate@example.com": true},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(results[0].Message)
	assert.Equal("Archive|10", results[0].Message.ID)
	assert.Nil(results[0].Message.Raw)
	require.NoError(results[0].Err)
}

func TestApplyFetchResultsDedupsUsingRawMessageIDWithoutEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBufferWithoutEnvelope(
			[]byte("Message-ID: <duplicate@example.com>\r\n\r\nbody"),
		),
	}
	c := Client{
		seenRFC822IDs: map[string]bool{"duplicate@example.com": true},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(results[0].Message)
	assert.Equal("Archive|10", results[0].Message.ID)
	assert.Nil(results[0].Message.Raw)
	require.NoError(results[0].Err)
}

func TestApplyFetchResultsMergesLabelsUsingRawMessageIDWithoutEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	raw := []byte("Message-ID: <shared@example.com> (comment)\r\n\r\nbody")
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBufferWithoutEnvelope(raw),
	}
	c := Client{
		msgIDToLabels: map[string][]string{
			"shared@example.com": {"Archive", "Projects"},
		},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(results[0].Message)
	assert.Equal([]string{"Archive", "Projects"}, results[0].Message.LabelIDs)
	assert.Equal(raw, results[0].Message.Raw)
	require.NoError(results[0].Err)
}

func TestApplyFetchResultsMergesLabelsWhenRawMessageIDHasRecoverableMIMEError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	raw := []byte("Message-ID: <shared@example.com>\r\nContent-Transfer-Encoding: i-dont-exist\r\n\r\nbody")
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBufferWithoutEnvelope(raw),
	}
	c := Client{
		msgIDToLabels: map[string][]string{
			"shared@example.com": {"Archive", "Projects"},
		},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(results[0].Message)
	assert.Equal([]string{"Archive", "Projects"}, results[0].Message.LabelIDs)
	assert.Equal(raw, results[0].Message.Raw)
	require.NoError(results[0].Err)
}

func TestApplyFetchResultsImportsWhenRawMessageIDMissingOrInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "missing",
			raw:  []byte("Subject: no message id\r\n\r\nbody"),
		},
		{
			name: "invalid header",
			raw:  []byte("broken header\r\n\r\nbody"),
		},
		{
			name: "invalid message id value",
			raw:  []byte("Message-ID: not a message id\r\n\r\nbody"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			results := newRawBatchResults([]string{"Archive|10"})
			uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
			chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
			msgs := []*imapclient.FetchMessageBuffer{
				fetchMessageBufferWithoutEnvelope(tt.raw),
			}
			c := Client{
				seenRFC822IDs: map[string]bool{"existing": true},
				msgIDToLabels: map[string][]string{"existing": {"Projects"}},
			}

			c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

			require.NotNil(results[0].Message)
			assert.Equal("Archive|10", results[0].Message.ID)
			assert.Equal([]string{"Archive"}, results[0].Message.LabelIDs)
			assert.Equal(tt.raw, results[0].Message.Raw)
			require.NoError(results[0].Err)
			assert.Equal(map[string]bool{"existing": true}, c.seenRFC822IDs)
		})
	}
}

func fetchMessageBuffer(messageID string, raw []byte) *imapclient.FetchMessageBuffer {
	return &imapclient.FetchMessageBuffer{
		UID:        imapapi.UID(10),
		Envelope:   &imapapi.Envelope{MessageID: messageID},
		RFC822Size: int64(len(raw)),
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Bytes: raw},
		},
	}
}

func fetchMessageBufferWithoutEnvelope(raw []byte) *imapclient.FetchMessageBuffer {
	return &imapclient.FetchMessageBuffer{
		UID:        imapapi.UID(10),
		RFC822Size: int64(len(raw)),
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Bytes: raw},
		},
	}
}

func startMalformedEnvelopeIMAPServer(t *testing.T, raw []byte) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	fetchCommand := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] synthetic server ready\r\n")
		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			line = strings.TrimSpace(line)
			tag, command, _ := strings.Cut(line, " ")
			upper := strings.ToUpper(command)
			switch {
			case strings.HasPrefix(upper, "LOGIN"):
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case strings.HasPrefix(upper, "SELECT"):
				_, _ = io.WriteString(conn,
					"* FLAGS (\\Seen)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 1]\r\n* OK [UIDNEXT 915]\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
			case strings.HasPrefix(upper, "UID FETCH"):
				fetchCommand <- upper
				if strings.Contains(upper, "ENVELOPE") {
					_, _ = io.WriteString(conn, "* 1 FETCH (UID 914 ENVELOPE BROKEN)\r\n")
				} else {
					_, _ = fmt.Fprintf(conn,
						"* 1 FETCH (UID 914 FLAGS () INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" RFC822.SIZE %d BODY[] {%d}\r\n",
						len(raw), len(raw))
					_, _ = conn.Write(raw)
					_, _ = io.WriteString(conn, ")\r\n")
				}
				_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
			case strings.HasPrefix(upper, "LOGOUT"):
				_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unsupported synthetic command\r\n", tag)
			}
		}
	}()
	return listener.Addr().String(), fetchCommand
}

// markConfirmedGone is what the chunk loops do once a second FETCH has also
// left a UID out. It is inlined here so these tests cover the same end state
// they covered before the recheck moved the decision out of the apply step.
func markConfirmedGone(
	c *Client,
	raw []gmailapi.RawMessageBatchResult,
	labels []gmailapi.MessageLabelsBatchResult,
	mailbox string,
	omitted []batchFetchItem,
) {
	for _, item := range omitted {
		if raw != nil {
			raw[item.idx].Err = errIMAPFetchResultMissing
		}
		if labels != nil {
			labels[item.idx].Err = errIMAPFetchResultMissing
		}
		c.forgetMembershipLocked(mailbox, item.uid)
	}
}

// TestMissingUIDDropsEarlierMembershipObservation covers the observation the
// label map records during enumeration. When a message is confirmed gone --
// left out of a FETCH response and then left out of the recheck -- nothing
// ingests it, so the stale observation must not survive to the durable commit,
// which would find no message row to map it to.
func TestMissingUIDDropsEarlierMembershipObservation(t *testing.T) {
	enumerated := func() []MembershipObservation {
		return []MembershipObservation{
			{Mailbox: "Archive", UID: 10, SourceMessageID: "Archive|10"},
			{Mailbox: "Archive", UID: 11, SourceMessageID: "Archive|11"},
		}
	}
	uidToIdx := map[imapapi.UID]int{
		imapapi.UID(10): 0,
		imapapi.UID(11): 1,
	}
	chunk := []batchFetchItem{
		{idx: 0, uid: imapapi.UID(10)},
		{idx: 1, uid: imapapi.UID(11)},
	}

	t.Run("raw fetch", func(t *testing.T) {
		c := Client{observedMemberships: enumerated()}
		results := newRawBatchResults([]string{"Archive|10", "Archive|11"})
		msgs := []*imapclient.FetchMessageBuffer{
			fetchMessageBuffer("message-10", []byte("raw-10")),
		}

		omitted := c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)
		markConfirmedGone(&c, results, nil, "Archive", omitted)

		require.ErrorIs(t, results[1].Err, errIMAPFetchResultMissing)
		assert.NotContains(t, observedUIDs(&c), uint32(11))
		assert.Contains(t, observedUIDs(&c), uint32(10))
	})

	t.Run("label fetch", func(t *testing.T) {
		c := Client{observedMemberships: enumerated()}
		results := newLabelBatchResults([]string{"Archive|10", "Archive|11"})
		msgs := []*imapclient.FetchMessageBuffer{
			fetchMessageBuffer(
				"message-10",
				[]byte("Message-ID: <message-10@example.com>\r\n\r\n"),
			),
		}

		omitted := c.applyLabelFetchResults(results, uidToIdx, "Archive", chunk, msgs)
		markConfirmedGone(&c, nil, results, "Archive", omitted)

		require.ErrorIs(t, results[1].Err, errIMAPFetchResultMissing)
		assert.NotContains(t, observedUIDs(&c), uint32(11))
		assert.Contains(t, observedUIDs(&c), uint32(10))
	})
}

func observedUIDs(c *Client) []uint32 {
	uids := make([]uint32, 0, len(c.observedMemberships))
	for _, observation := range c.observedMemberships {
		uids = append(uids, observation.UID)
	}
	return uids
}

// TestReturnedUIDWithoutHeadersKeepsMembershipObservation is the other side of
// TestMissingUIDDropsEarlierMembershipObservation. The server returned the
// UID, so the message is still in the mailbox. Classifying that as gone would
// let the run acknowledge a live message and drop its observation, and a
// mailbox that resets its memberships would then delete the row and tombstone
// the message.
func TestReturnedUIDWithoutHeadersKeepsMembershipObservation(t *testing.T) {
	c := Client{observedMemberships: []MembershipObservation{
		{Mailbox: "Archive", UID: 10, SourceMessageID: "Archive|10"},
	}}
	results := newLabelBatchResults([]string{"Archive|10"})
	msgs := []*imapclient.FetchMessageBuffer{
		{UID: imapapi.UID(10), Envelope: &imapapi.Envelope{MessageID: "message-10"}},
	}

	c.applyLabelFetchResults(
		results,
		map[imapapi.UID]int{imapapi.UID(10): 0},
		"Archive",
		[]batchFetchItem{{idx: 0, uid: imapapi.UID(10)}},
		msgs,
	)

	require.ErrorIs(t, results[0].Err, errIMAPLabelBodyMissing)
	require.NotErrorIs(t, results[0].Err, gmailapi.ErrMessageGone)
	assert.Contains(t, observedUIDs(&c), uint32(10))
}

// TestOmittedUIDIsRecheckedBeforeItCountsAsGone is the reason the recheck
// exists. The server drops one live UID from the first FETCH response and
// returns it on the next, which is a response the server got wrong rather than
// a message that left the mailbox.
//
// Believing the first omission loses the message on a QRESYNC account: the run
// acknowledges the UID, stores no membership for it, and still advances
// HIGHESTMODSEQ, so no later CHANGEDSINCE fetch reports it again. Only the
// paths that reconcile a mailbox by message count recover on their own, so the
// omission must not be believed the first time on any path.
func TestOmittedUIDIsRecheckedBeforeItCountsAsGone(t *testing.T) {
	t.Run("label fetch", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		addr, _ := testutil.StartIMAPMemServerOmittingUIDOnce(
			t, map[string]int{"INBOX": 2}, "INBOX", imapapi.UID(2))
		client := newTestClient(t, addr)

		results, err := client.GetMessageLabelsBatch(
			t.Context(), []string{"INBOX|1", "INBOX|2"})

		require.NoError(err)
		require.Len(results, 2)
		require.NoError(results[0].Err)
		require.NoError(results[1].Err,
			"a UID the server returns on the recheck is a live message")
		assert.Contains(observedUIDs(client), uint32(2),
			"a rechecked message keeps the membership the commit publishes")
	})

	t.Run("raw fetch", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		addr, _ := testutil.StartIMAPMemServerOmittingUIDOnce(
			t, map[string]int{"INBOX": 2}, "INBOX", imapapi.UID(2))
		client := newTestClient(t, addr)

		results, err := client.GetMessagesRawBatchWithErrors(
			t.Context(), []string{"INBOX|1", "INBOX|2"})

		require.NoError(err)
		require.Len(results, 2)
		require.NoError(results[0].Err)
		require.NoError(results[1].Err,
			"a UID the server returns on the recheck is a live message")
		require.NotNil(results[1].Message)
		assert.Contains(observedUIDs(client), uint32(2))
	})
}

// startOmitThenDieIMAPServer answers one FETCH with a response that leaves
// uid out, then stops serving and closes its listener. The recheck that
// follows therefore hits a dead socket and cannot reconnect.
func startOmitThenDieIMAPServer(t *testing.T, uid imapapi.UID) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] synthetic server ready\r\n")
		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			tag, command, _ := strings.Cut(strings.TrimSpace(line), " ")
			upper := strings.ToUpper(command)
			switch {
			case strings.HasPrefix(upper, "LOGIN"):
				_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
			case strings.HasPrefix(upper, "SELECT"):
				_, _ = io.WriteString(conn,
					"* FLAGS (\\Seen)\r\n* 2 EXISTS\r\n* OK [UIDVALIDITY 1]\r\n* OK [UIDNEXT 3]\r\n")
				_, _ = fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
			case strings.HasPrefix(upper, "UID FETCH"):
				// Answer with every requested UID except the one under test,
				// then refuse to serve anything further.
				_, _ = fmt.Fprintf(conn,
					"* 1 FETCH (UID %d FLAGS () INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" "+
						"RFC822.SIZE 5 BODY[] {5}\r\nhello)\r\n", uid-1)
				_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
				_ = listener.Close()
				return
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unsupported synthetic command\r\n", tag)
			}
		}
	}()
	return addr
}

// TestRecheckReconnectFailureEndsTheBatch covers the invariant the recheck must
// not break. fetchChunk reports a failed reconnect as fatal, and a fatal
// failure has already cleared c.conn, so the batch cannot continue: the next
// chunk would dereference a nil connection. The recheck has to propagate that
// the way the first fetch of a chunk already does.
func TestRecheckReconnectFailureEndsTheBatch(t *testing.T) {
	require := require.New(t)
	addr := startOmitThenDieIMAPServer(t, imapapi.UID(2))
	client := newTestClient(t, addr)

	_, err := client.GetMessagesRawBatchWithErrors(
		t.Context(), []string{"INBOX|1", "INBOX|2"})

	require.Error(err, "a failed reconnect during the recheck must end the batch")
}
