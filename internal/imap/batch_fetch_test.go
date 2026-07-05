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

func TestRawBatchMessagesWithErrorPreservesPartialResults(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	errBatch := errors.New("batch stopped")
	msg0 := &gmailapi.RawMessage{ID: "Archive|10", Raw: []byte("raw-10")}
	msg2 := &gmailapi.RawMessage{ID: "Archive|12", Raw: []byte("raw-12")}
	results := []gmailapi.RawMessageBatchResult{
		{ID: "Archive|10", Message: msg0},
		{ID: "Archive|11", Err: errors.New("fetch failed")},
		{ID: "Archive|12", Message: msg2},
	}

	messages, err := rawBatchMessagesWithError(results, errBatch)

	require.ErrorIs(err, errBatch)
	require.Len(messages, 3)
	assert.Same(msg0, messages[0])
	assert.Nil(messages[1])
	assert.Same(msg2, messages[2])
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
		fetchMessageBuffer(imapapi.UID(10), "message-10", []byte("raw-10")),
	}

	var c Client
	c.applyFetchResults(results, uidToIdx, "Archive", chunk, msgs)

	require.NotNil(results[0].Message)
	assert.Equal("Archive|10", results[0].Message.ID)
	assert.Equal([]byte("raw-10"), results[0].Message.Raw)
	require.NoError(results[0].Err)
	assert.Nil(results[1].Message)
	require.ErrorIs(results[1].Err, errIMAPFetchResultMissing)
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
	require := require.New(t)
	assert := assert.New(t)
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

	require.NotNil(results[0].Message)
	assert.Equal("Archive|10", results[0].Message.ID)
	assert.Nil(results[0].Message.Raw)
	require.NoError(results[0].Err)
}

func TestRawBatchFetchOptionsDoNotRequestEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	opts := rawBatchFetchOptions()

	assert.True(opts.UID)
	assert.False(opts.Envelope)
	assert.True(opts.InternalDate)
	assert.True(opts.RFC822Size)
	require.Len(opts.BodySection, 1)
	assert.True(opts.BodySection[0].Peek)
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

func fetchMessageBufferWithoutEnvelope(raw []byte) *imapclient.FetchMessageBuffer {
	return &imapclient.FetchMessageBuffer{
		UID:        imapapi.UID(10),
		RFC822Size: int64(len(raw)),
		BodySection: []imapclient.FetchBodySectionBuffer{
			{Bytes: raw},
		},
	}
}
