package store

import (
	"context"
	"database/sql/driver"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type catalogRevisionCtxKey struct{}

func TestCommitBumpsCatalogRevisionWithTheCallersContext(t *testing.T) {
	base := newRFC822IDBackfillBackendStore(t)
	var bumpValue any
	gate := &meetingProjectionGate{before: func(ctx context.Context, query string, args []driver.NamedValue) error {
		if strings.Contains(query, "INSERT INTO archive_metadata") && slices.ContainsFunc(args, func(arg driver.NamedValue) bool { return arg.Value == accountCatalogRevisionKey }) {
			bumpValue = ctx.Value(catalogRevisionCtxKey{})
		}
		return nil
	}}
	st := gatedMeetingProjectionStore(t, base, gate)
	source, err := st.GetOrCreateSource("mbox", "me@example.net")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Thread")
	require.NoError(t, err)
	bumpValue = nil
	ctx := context.WithValue(t.Context(), catalogRevisionCtxKey{}, "caller")
	_, err = st.UpsertMessageContext(ctx, &Message{
		SourceID: source.ID, ConversationID: conv, SourceMessageID: "m-1", MessageType: "email", SizeEstimate: 100,
	})
	require.NoError(t, err)
	assert.Equal(t, "caller", bumpValue, "the bump runs under the caller's context")
}
