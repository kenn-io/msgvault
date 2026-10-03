package imap

import (
	"errors"
	"fmt"
	"net"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/testutil"
)

// Wrap the real memory session only to inject provider restrictions and failures.
type keywordTestSession struct {
	imapserver.Session

	permanent    []imapapi.Flag
	rejectRemove bool
	omitFlags    bool
}

func (s *keywordTestSession) Select(name string, opts *imapapi.SelectOptions) (*imapapi.SelectData, error) {
	data, err := s.Session.Select(name, opts)
	if err == nil && s.permanent != nil {
		data.PermanentFlags = s.permanent
	}
	if err != nil {
		return nil, fmt.Errorf("select test mailbox: %w", err)
	}
	return data, nil
}
func (s *keywordTestSession) Store(w *imapserver.FetchWriter, set imapapi.NumSet, flags *imapapi.StoreFlags, opts *imapapi.StoreOptions) error {
	if s.rejectRemove && flags.Op == imapapi.StoreFlagsDel {
		return errors.New("synthetic remove rejection")
	}
	if err := s.Session.Store(w, set, flags, opts); err != nil {
		return fmt.Errorf("store test keyword flags: %w", err)
	}
	return nil
}
func (s *keywordTestSession) Fetch(w *imapserver.FetchWriter, set imapapi.NumSet, opts *imapapi.FetchOptions) error {
	if s.omitFlags {
		options := *opts
		options.Flags = false
		opts = &options
	}
	if err := s.Session.Fetch(w, set, opts); err != nil {
		return fmt.Errorf("fetch test keyword flags: %w", err)
	}
	return nil
}
func newKeywordTestClient(t *testing.T, permanent []imapapi.Flag, rejectRemove, omitFlags bool) (*Client, KeywordIdentity) {
	t.Helper()
	require := require.New(t)
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(user.Create("INBOX", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	mem := imapmemserver.New()
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &keywordTestSession{Session: mem.NewSession(), permanent: permanent, rejectRemove: rejectRemove, omitFlags: omitFlags}, nil, nil
	}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := newTestClient(t, listener.Addr().String())
	var data *imapapi.SelectData
	err = c.withConn(t.Context(), func(conn *imapclient.Client) error {
		var err error
		data, err = conn.Select("INBOX", nil).Wait()
		if err != nil {
			return fmt.Errorf("select keyword fixture: %w", err)
		}
		return nil
	})
	require.NoError(err)
	return c, KeywordIdentity{Mailbox: "INBOX", UIDValidity: data.UIDValidity, UID: 1}
}
func TestMessageKeywordsPreservesFlagsAndSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, id := newKeywordTestClient(t, nil, false, false)
	c.mu.Lock()
	var set imapapi.UIDSet
	set.AddNum(1)
	_, err := c.conn.Store(set, &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Old", "Unrelated"}}, nil).Collect()
	c.mu.Unlock()
	require.NoError(err)
	change := emailtags.Change{Add: []string{"Next"}, Remove: []string{"old"}, DryRun: true}
	preview, err := c.MessageKeywords(t.Context(), id, &change)
	require.NoError(err)
	assert.True(preview.DryRun)
	assert.Contains(preview.Tags, "Next")
	read, err := c.MessageKeywords(t.Context(), id, nil)
	require.NoError(err)
	assert.True(emailtags.Contains(read.Tags, "Old", true))
	change.DryRun = false
	got, err := c.MessageKeywords(t.Context(), id, &change)
	require.NoError(err)
	assert.True(got.Verified)
	assert.True(emailtags.Verify(got.Tags, change, true))
	assert.True(emailtags.Contains(got.Tags, "Unrelated", true))
	again, err := c.MessageKeywords(t.Context(), id, &change)
	require.NoError(err)
	assert.ElementsMatch(got.Tags, again.Tags)
	raw, err := c.GetMessageRaw(t.Context(), "INBOX|1")
	require.NoError(err)
	require.NotNil(raw)
	observations := c.ObservedMemberships()
	require.NotEmpty(observations)
	flags := observations[len(observations)-1].Flags
	assert.Contains(flags, string(imapapi.FlagSeen))
	assert.Contains(flags, string(imapapi.FlagFlagged))
	assert.True(emailtags.Contains(flags, "Next", true))
}
func TestMessageKeywordsRejectsStaleAndInvalidTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, id := newKeywordTestClient(t, nil, false, false)
	empty, err := c.MessageKeywords(t.Context(), id, nil)
	require.NoError(err)
	assert.Empty(empty.Tags)
	for _, tag := range []string{"\\Seen", "\\Deleted", "two words", "bad\r\nflag", "x]", "x\\flag"} {
		_, err := c.MessageKeywords(t.Context(), id, &emailtags.Change{Add: []string{tag}})
		require.Error(err)
	}
	_, err = c.MessageKeywords(t.Context(), id, &emailtags.Change{Add: []string{"Work"}, Remove: []string{"work"}})
	require.Error(err)
	stale := id
	stale.UIDValidity++
	_, err = c.MessageKeywords(t.Context(), stale, &emailtags.Change{Add: []string{"Next"}})
	require.Error(err)
	missing := id
	missing.UID = 99
	_, err = c.MessageKeywords(t.Context(), missing, &emailtags.Change{Add: []string{"Next"}})
	require.Error(err)
	read, err := c.MessageKeywords(t.Context(), id, nil)
	require.NoError(err)
	assert.Empty(read.Tags)
}
func TestMessageKeywordsRestrictedAndIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name      string
		permanent []imapapi.Flag
		omit      bool
		readFail  bool
	}{
		{"no writable keywords", []imapapi.Flag{imapapi.FlagSeen}, false, false},
		{"missing FLAGS", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			c, id := newKeywordTestClient(t, tc.permanent, false, tc.omit)
			_, err := c.MessageKeywords(t.Context(), id, nil)
			if tc.readFail {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			_, err = c.MessageKeywords(t.Context(), id, &emailtags.Change{Add: []string{"Next"}})
			require.Error(err)
		})
	}
}
func TestMessageKeywordsPartialWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, id := newKeywordTestClient(t, nil, true, false)
	_, err := c.MessageKeywords(t.Context(), id, &emailtags.Change{Add: []string{"Old"}})
	require.NoError(err)
	result, err := c.MessageKeywords(t.Context(), id, &emailtags.Change{Add: []string{"Next"}, Remove: []string{"Old"}})
	require.Error(err)
	require.NotNil(result)
	assert.True(emailtags.Contains(result.Tags, "Next", true))
	assert.True(emailtags.Contains(result.Tags, "Old", true))
	assert.False(result.Verified)
	var failure *emailtags.Error
	require.ErrorAs(err, &failure)
	assert.Equal(result, failure.Result)
}
