package importer

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSenderAuthImportedOwnerAddress(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imported-mail", "owner@example.test")
	must.NoError(err)
	must.NoError(st.AddAccountIdentity(source.ID, source.Identifier, "account-identifier"))
	raw := []byte("From: owner@example.test\r\nTo: reader@example.test\r\nMessage-ID: <receipt@example.test>\r\nAuthentication-Results: mx.test; dmarc=fail header.from=example.test\r\n\r\nBody")
	must.NoError(IngestRawMessage(t.Context(), st, source.ID, source.Identifier, "", nil, "eml-fixture", "receipt", raw, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), slog.New(slog.DiscardHandler)))
	var id int64
	var sourceOwned, identityOwned bool
	must.NoError(st.DB().QueryRow(st.Rebind(`SELECT id,source_is_from_me,identity_is_from_me FROM messages WHERE source_id=?`), source.ID).Scan(&id, &sourceOwned, &identityOwned))
	checks.False(sourceOwned)
	checks.True(identityOwned)
	e := query.NewEngine(st.DB(), st.IsPostgreSQL())
	query.ConfigureSenderAuth(e, map[int64]senderauth.TrustSet{source.ID: {"mx.test"}})
	for _, legacy := range []bool{false, true} {
		if legacy {
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_is_from_me=TRUE,identity_is_from_me=FALSE WHERE id=?`), id)
			must.NoError(err)
		}
		detail, err := e.GetMessage(t.Context(), id)
		must.NoError(err)
		must.NotNil(detail)
		must.NotNil(detail.SenderAuth)
		checks.False(detail.SenderAuth.Verified)
		checks.Equal(senderauth.Fail, detail.SenderAuth.DMARC)
		checks.Equal(senderauth.ProviderHeader, detail.SenderAuth.Basis)
	}
}
