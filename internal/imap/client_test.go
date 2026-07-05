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
	require := require.New(t)
	assert := assert.New(t)
	criteria := enumerateMailboxSearchCriteria(time.Time{}, time.Time{}, 0)

	require.NotNil(criteria)
	require.Len(criteria.UID, 1)
	assert.Equal("1:*", criteria.UID[0].String())
	assert.True(criteria.Since.IsZero())
	assert.True(criteria.Before.IsZero())
}

func TestEnumerateMailboxSearchCriteriaPreservesDateFilters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	since := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, time.February, 3, 0, 0, 0, 0, time.UTC)

	criteria := enumerateMailboxSearchCriteria(since, before, 0)

	require.NotNil(criteria)
	require.Len(criteria.UID, 1)
	assert.Equal("1:*", criteria.UID[0].String())
	assert.Equal(since, criteria.Since)
	assert.Equal(before, criteria.Before)
}

func TestEnumerateMailboxSearchCriteriaUsesMinimumUID(t *testing.T) {
	criteria := enumerateMailboxSearchCriteria(time.Time{}, time.Time{}, 501)

	require.NotNil(t, criteria)
	require.Len(t, criteria.UID, 1)
	assert.Equal(t, "501:*", criteria.UID[0].String())
}

func TestMessageIDHeaderFetchOptionsDoNotRequestEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	opts := messageIDHeaderFetchOptions()

	assert.True(opts.UID)
	assert.False(opts.Envelope)
	require.Len(opts.BodySection, 1)
	assert.True(opts.BodySection[0].Peek)
	assert.Equal(imapapi.PartSpecifierHeader, opts.BodySection[0].Specifier)
	assert.Equal([]string{"Message-ID"}, opts.BodySection[0].HeaderFields)
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
