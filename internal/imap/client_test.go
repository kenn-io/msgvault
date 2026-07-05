package imap

import (
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnumerateMailboxSearchCriteriaConstrainsUIDRange(t *testing.T) {
	criteria := enumerateMailboxSearchCriteria(time.Time{}, time.Time{}, 0)

	require.NotNil(t, criteria)
	require.Len(t, criteria.UID, 1)
	assert.Equal(t, "1:*", criteria.UID[0].String())
	assert.True(t, criteria.Since.IsZero())
	assert.True(t, criteria.Before.IsZero())
}

func TestEnumerateMailboxSearchCriteriaPreservesDateFilters(t *testing.T) {
	since := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, time.February, 3, 0, 0, 0, 0, time.UTC)

	criteria := enumerateMailboxSearchCriteria(since, before, 0)

	require.NotNil(t, criteria)
	require.Len(t, criteria.UID, 1)
	assert.Equal(t, "1:*", criteria.UID[0].String())
	assert.Equal(t, since, criteria.Since)
	assert.Equal(t, before, criteria.Before)
}

func TestEnumerateMailboxSearchCriteriaUsesMinimumUID(t *testing.T) {
	criteria := enumerateMailboxSearchCriteria(time.Time{}, time.Time{}, 501)

	require.NotNil(t, criteria)
	require.Len(t, criteria.UID, 1)
	assert.Equal(t, "501:*", criteria.UID[0].String())
}

func TestMessageIDHeaderFetchOptionsDoNotRequestEnvelope(t *testing.T) {
	opts := messageIDHeaderFetchOptions()

	assert.True(t, opts.UID)
	assert.False(t, opts.Envelope)
	require.Len(t, opts.BodySection, 1)
	assert.True(t, opts.BodySection[0].Peek)
	assert.Equal(t, imapapi.PartSpecifierHeader, opts.BodySection[0].Specifier)
	assert.Equal(t, []string{"Message-ID"}, opts.BodySection[0].HeaderFields)
}

func TestMessageIDsFromHeaderFetchResultsParsesMessageIDHeaders(t *testing.T) {
	msgs := []*imapclient.FetchMessageBuffer{
		{
			UID: imapapi.UID(10),
			BodySection: []imapclient.FetchBodySectionBuffer{
				{Bytes: []byte("Message-ID: <one@example.com> (comment)\r\n\r\n")},
			},
		},
		{
			UID: imapapi.UID(11),
			BodySection: []imapclient.FetchBodySectionBuffer{
				{Bytes: []byte("Message-ID: not a message id\r\n\r\n")},
			},
		},
		{
			UID: imapapi.UID(12),
			BodySection: []imapclient.FetchBodySectionBuffer{
				{Bytes: []byte("Subject: no message id\r\n\r\n")},
			},
		},
	}

	got := messageIDsFromHeaderFetchResults(msgs)

	assert.Equal(t, map[string]bool{"one@example.com": true}, got)
}
