package mcpevents

import (
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// The source resolver must not authorize source/inbox enumeration, accept a
// caller-selected owner, or revive an observed lifetime after identity reuse.
func TestImportSourceReferenceResolverRequiresOwnerAndExactSource(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	f := storetest.New(t)
	svc, err := New(t.Context(), f.Store, Options{OwnerKey: "synthetic-import-reference-owner"})
	require.NoError(err)
	id := strconv.FormatInt(f.Source.ID, 10)
	for _, test := range []struct {
		name, principal string
		query           ResourceQuery
	}{
		{"different owner", "owner:synthetic-other", ResourceQuery{Kind: "source", SourceID: id}},
		{"all sources", svc.principal, ResourceQuery{Kind: "source"}},
		{"conversations", svc.principal, ResourceQuery{Kind: "conversation", SourceID: id}},
		{"inboxes", svc.principal, ResourceQuery{Kind: "inbox", SourceID: id}},
		{"noncanonical ID", svc.principal, ResourceQuery{Kind: "source", SourceID: "0" + id}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := svc.DiscoverResources(t.Context(), test.principal, test.query)
			Require.Error(t, err)
		})
	}
	var before int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_source_resources`).Scan(&before))
	assert.Zero(before, "denied requests cannot allocate resource identities")
	resolve := func() Resource {
		page, err := svc.DiscoverResources(t.Context(), svc.principal, ResourceQuery{Kind: "source", SourceID: id})
		require.NoError(err)
		require.Len(page.Resources, 1)
		return page.Resources[0]
	}
	observed := resolve()
	assert.NotEmpty(observed.Reference)
	assert.Empty(observed.Capabilities.Events, "default-off capture stays disabled")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET display_name=? WHERE id=?`), "Synthetic renamed import", f.Source.ID)
	require.NoError(err)
	assert.Equal(observed.Reference, resolve().Reference, "presentation changes preserve identity")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), "synthetic-replacement@example.net", f.Source.ID)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), f.Source.Identifier, f.Source.ID)
	require.NoError(err)
	assert.NotEqual(observed.Reference, resolve().Reference, "identity changes away and back cannot revive a reference")
}
