package imap

import (
	"errors"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gmailapi "go.kenn.io/msgvault/internal/gmail"
)

func TestNewRawBatchResultsKeepsInputIDs(t *testing.T) {
	results := newRawBatchResults([]string{"Archive|10", "Archive|11"})

	require.Len(t, results, 2)
	assert.Equal(t, "Archive|10", results[0].ID)
	assert.Nil(t, results[0].Message)
	assert.Nil(t, results[0].Err)
	assert.Equal(t, "Archive|11", results[1].ID)
	assert.Nil(t, results[1].Message)
	assert.Nil(t, results[1].Err)
}

func TestMarkRawBatchErrorMarksOnlyRequestedItems(t *testing.T) {
	errFetch := errors.New("fetch failed")
	results := newRawBatchResults([]string{"Archive|10", "Archive|11", "Archive|12"})
	items := []batchFetchItem{
		{idx: 0, uid: imapapi.UID(10)},
		{idx: 2, uid: imapapi.UID(12)},
	}

	markRawBatchError(results, items, errFetch)

	assert.True(t, results[0].Err == errFetch)
	assert.Nil(t, results[1].Err)
	assert.True(t, results[2].Err == errFetch)
}

func TestRawBatchMessagesDropsPerItemErrorsForLegacyCallers(t *testing.T) {
	msg0 := &gmailapi.RawMessage{ID: "Archive|10", Raw: []byte("raw-10")}
	msg2 := &gmailapi.RawMessage{ID: "Archive|12", Raw: []byte("raw-12")}
	results := []gmailapi.RawMessageBatchResult{
		{ID: "Archive|10", Message: msg0},
		{ID: "Archive|11", Err: errors.New("fetch failed")},
		{ID: "Archive|12", Message: msg2},
	}

	messages := rawBatchMessages(results)

	require.Len(t, messages, 3)
	assert.Same(t, msg0, messages[0])
	assert.Nil(t, messages[1])
	assert.Same(t, msg2, messages[2])
}

func TestRawBatchMessagesWithErrorPreservesPartialResults(t *testing.T) {
	errBatch := errors.New("batch stopped")
	msg0 := &gmailapi.RawMessage{ID: "Archive|10", Raw: []byte("raw-10")}
	msg2 := &gmailapi.RawMessage{ID: "Archive|12", Raw: []byte("raw-12")}
	results := []gmailapi.RawMessageBatchResult{
		{ID: "Archive|10", Message: msg0},
		{ID: "Archive|11", Err: errors.New("fetch failed")},
		{ID: "Archive|12", Message: msg2},
	}

	messages, err := rawBatchMessagesWithError(results, errBatch)

	require.ErrorIs(t, err, errBatch)
	require.Len(t, messages, 3)
	assert.Same(t, msg0, messages[0])
	assert.Nil(t, messages[1])
	assert.Same(t, msg2, messages[2])
}

func TestApplyFetchResultsMarksMissingUIDs(t *testing.T) {
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
		fetchMessageBuffer(imapapi.UID(10), "message-10", []byte("raw-10")),
	}

	var c Client
	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(t, results[0].Message)
	assert.Equal(t, "Archive|10", results[0].Message.ID)
	assert.Equal(t, []byte("raw-10"), results[0].Message.Raw)
	assert.Nil(t, results[0].Err)
	assert.Nil(t, results[1].Message)
	require.ErrorIs(t, results[1].Err, errIMAPFetchResultMissing)
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
			msg:  fetchMessageBuffer(imapapi.UID(10), "message-10", nil),
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
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBuffer(
			imapapi.UID(10),
			"duplicate@example.com",
			[]byte("Message-ID: <duplicate@example.com>\r\n\r\nbody"),
		),
	}
	c := Client{
		seenRFC822IDs: map[string]bool{"duplicate@example.com": true},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(t, results[0].Message)
	assert.Equal(t, "Archive|10", results[0].Message.ID)
	assert.Nil(t, results[0].Message.Raw)
	assert.Nil(t, results[0].Err)
}

func TestRawBatchFetchOptionsDoNotRequestEnvelope(t *testing.T) {
	opts := rawBatchFetchOptions()

	assert.True(t, opts.UID)
	assert.False(t, opts.Envelope)
	assert.True(t, opts.InternalDate)
	assert.True(t, opts.RFC822Size)
	require.Len(t, opts.BodySection, 1)
	assert.True(t, opts.BodySection[0].Peek)
}

func TestApplyFetchResultsDedupsUsingRawMessageIDWithoutEnvelope(t *testing.T) {
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBufferWithoutEnvelope(
			imapapi.UID(10),
			[]byte("Message-ID: <duplicate@example.com>\r\n\r\nbody"),
		),
	}
	c := Client{
		seenRFC822IDs: map[string]bool{"duplicate@example.com": true},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(t, results[0].Message)
	assert.Equal(t, "Archive|10", results[0].Message.ID)
	assert.Nil(t, results[0].Message.Raw)
	assert.Nil(t, results[0].Err)
}

func TestApplyFetchResultsMergesLabelsUsingRawMessageIDWithoutEnvelope(t *testing.T) {
	results := newRawBatchResults([]string{"Archive|10"})
	uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
	chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
	raw := []byte("Message-ID: <shared@example.com>\r\n\r\nbody")
	msgs := []*imapclient.FetchMessageBuffer{
		fetchMessageBufferWithoutEnvelope(imapapi.UID(10), raw),
	}
	c := Client{
		msgIDToLabels: map[string][]string{
			"shared@example.com": {"Archive", "Projects"},
		},
	}

	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(t, results[0].Message)
	assert.Equal(t, []string{"Archive", "Projects"}, results[0].Message.LabelIDs)
	assert.Equal(t, raw, results[0].Message.Raw)
	assert.Nil(t, results[0].Err)
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
			results := newRawBatchResults([]string{"Archive|10"})
			uidToIdx := map[imapapi.UID]int{imapapi.UID(10): 0}
			chunk := []batchFetchItem{{idx: 0, uid: imapapi.UID(10)}}
			msgs := []*imapclient.FetchMessageBuffer{
				fetchMessageBufferWithoutEnvelope(imapapi.UID(10), tt.raw),
			}
			c := Client{
				seenRFC822IDs: map[string]bool{"existing": true},
				msgIDToLabels: map[string][]string{"existing": {"Projects"}},
			}

			c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

			require.NotNil(t, results[0].Message)
			assert.Equal(t, "Archive|10", results[0].Message.ID)
			assert.Equal(t, []string{"Archive"}, results[0].Message.LabelIDs)
			assert.Equal(t, tt.raw, results[0].Message.Raw)
			assert.Nil(t, results[0].Err)
			assert.Equal(t, map[string]bool{"existing": true}, c.seenRFC822IDs)
		})
	}
}

func fetchMessageBuffer(uid imapapi.UID, messageID string, raw []byte) *imapclient.FetchMessageBuffer {
	return &imapclient.FetchMessageBuffer{
		UID:        uid,
		Envelope:   &imapapi.Envelope{MessageID: messageID},
		RFC822Size: int64(len(raw)),
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Bytes: raw},
		},
	}
}

func fetchMessageBufferWithoutEnvelope(uid imapapi.UID, raw []byte) *imapclient.FetchMessageBuffer {
	return &imapclient.FetchMessageBuffer{
		UID:        uid,
		RFC822Size: int64(len(raw)),
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Bytes: raw},
		},
	}
}
