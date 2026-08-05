package imap

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type specialUseListSession struct {
	imapserver.Session

	returnSpecialUse *atomic.Bool
}

type legacyListSession struct {
	imapserver.Session
}

func (s *legacyListSession) List(
	w *imapserver.ListWriter,
	_ string,
	_ []string,
	options *imapapi.ListOptions,
) error {
	if options.ReturnSpecialUse {
		return errors.New("extended LIST is unsupported")
	}
	if err := w.WriteList(&imapapi.ListData{
		Delim:   '/',
		Mailbox: "INBOX",
	}); err != nil {
		return fmt.Errorf("write LIST response: %w", err)
	}
	return nil
}

func (s *specialUseListSession) List(
	w *imapserver.ListWriter,
	_ string,
	_ []string,
	options *imapapi.ListOptions,
) error {
	s.returnSpecialUse.Store(options.ReturnSpecialUse)
	attrs := []imapapi.MailboxAttr(nil)
	if options.ReturnSpecialUse {
		attrs = append(attrs, imapapi.MailboxAttrSent)
	}
	if err := w.WriteList(&imapapi.ListData{
		Attrs:   attrs,
		Delim:   '/',
		Mailbox: "Envoyes",
	}); err != nil {
		return fmt.Errorf("write LIST response: %w", err)
	}
	return nil
}

func TestListLabelsRequestsSpecialUseForLocalizedSentMailbox(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	user := imapmemserver.NewUser("owner@example.test", "test-password")
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	var returnSpecialUse atomic.Bool
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &specialUseListSession{
				Session:          memServer.NewSession(),
				returnSpecialUse: &returnSpecialUse,
			}, nil, nil
		},
		Caps: imapapi.CapSet{
			imapapi.CapIMAP4rev1:    {},
			imapapi.CapListExtended: {},
			imapapi.CapSpecialUse:   {},
		},
		InsecureAuth: true,
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: "owner@example.test",
	}, "test-password")
	t.Cleanup(func() { _ = client.Close() })

	labels, err := client.ListLabels(t.Context())
	require.NoError(err)
	require.Len(labels, 1)
	assert.True(returnSpecialUse.Load(), "LIST must explicitly request RETURN (SPECIAL-USE)")
	assert.Equal("Envoyes", labels[0].Name)
	assert.Equal("sent", labels[0].SystemRole)
}

func TestListMailboxesUsesBasicListWithoutSpecialUseCapability(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	user := imapmemserver.NewUser("owner@example.test", "test-password")
	memServer := imapmemserver.New()
	memServer.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &legacyListSession{Session: memServer.NewSession()}, nil, nil
		},
		Caps: imapapi.CapSet{
			imapapi.CapIMAP4rev1: {},
		},
		InsecureAuth: true,
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(err)
	port, err := strconv.Atoi(portText)
	require.NoError(err)
	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: "owner@example.test",
	}, "test-password")
	t.Cleanup(func() { _ = client.Close() })

	labels, err := client.ListLabels(t.Context())
	require.NoError(err, "legacy label listing")
	require.Len(labels, 1)
	assert.Equal("INBOX", labels[0].Name)

	var mailboxes []string
	err = client.withConn(t.Context(), func(*imapclient.Client) error {
		var listErr error
		mailboxes, listErr = client.listMailboxesLocked()
		return listErr
	})
	require.NoError(err, "legacy mailbox listing")
	assert.Equal([]string{"INBOX"}, mailboxes)
}

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

func TestAddMessageIDsFromHeaderFetchResultsParsesMessageIDHeaders(t *testing.T) {
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

	got := map[string]bool{"existing@example.com": true}
	addMessageIDsFromHeaderFetchResults(got, msgs)

	assert.Equal(t, map[string]bool{
		"existing@example.com": true,
		"one@example.com":      true,
	}, got)
}
