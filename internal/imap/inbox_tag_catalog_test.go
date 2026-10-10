package imap

import (
	"fmt"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxIMAPTagCatalogExistingKeywords(t *testing.T) {
	for _, tc := range []struct {
		name                string
		permanent, existing []imapapi.Flag
		foreign             bool
		want                []emailtags.Tag
	}{
		{name: "named persistent", permanent: []imapapi.Flag{imapapi.FlagSeen, "Todo", "Watch"}, existing: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagFlagged, "Todo", "Transient"}, want: []emailtags.Tag{{ID: "Todo", Name: "Todo"}, {ID: "Watch", Name: "Watch"}}},
		{name: "wildcard existing only", permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagWildcard}, existing: []imapapi.Flag{imapapi.FlagFlagged, "Todo"}, want: []emailtags.Tag{{ID: "todo", Name: "todo"}}},
		{name: "nonpersistent excluded", permanent: []imapapi.Flag{imapapi.FlagSeen}, existing: []imapapi.Flag{"Transient"}, want: []emailtags.Tag{}},
		{name: "no keywords", permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagWildcard}, want: []emailtags.Tag{}},
		{name: "foreign source", permanent: []imapapi.Flag{imapapi.FlagSeen, imapapi.FlagWildcard}, foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var stores, creates, writable atomic.Int64
			client, id := newKeywordTestClientFor(t, keywordTestSession{permanent: tc.permanent, stores: &stores, creates: &creates, writableSelects: &writable})
			if len(tc.existing) > 0 {
				requirements.NoError(client.withConn(t.Context(), func(conn *imapclient.Client) error {
					_, err := conn.Store(imapapi.UIDSetNum(imapapi.UID(id.UID)), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: tc.existing}, nil).Collect()
					if err != nil {
						return fmt.Errorf("seed native IMAP flags: %w", err)
					}
					return nil
				}))
			}
			source, target := inboxIMAPBinding(client, id)
			provider := NewInboxProvider(client, source)
			before, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			requirements.NoError(err)
			storeBefore, writableBefore := stores.Load(), writable.Load()
			catalog, ok := any(provider).(inboxcontrol.TagCatalogProvider)
			requirements.True(ok, "IMAP must expose existing native keyword identities for GTD mappings")
			if tc.foreign {
				source.SourceID++
			}
			tags, err := catalog.TagCatalog(t.Context(), source)
			if tc.foreign {
				require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			} else {
				requirements.NoError(err)
				assertions.Equal(tc.want, tags)
			}
			after, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			requirements.NoError(err)
			assertions.Equal(before.Flags, after.Flags)
			assertions.Equal(storeBefore, stores.Load(), "catalog must not issue STORE")
			assertions.Zero(creates.Load(), "catalog must not issue CREATE")
			assertions.Equal(writableBefore, writable.Load(), "catalog must use read-only selection")
		})
	}
}
