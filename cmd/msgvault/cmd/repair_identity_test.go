package cmd

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// repair-identity must confirm the own-address identity for exactly the
// email-shaped, identity-less sources of the requested type, and be idempotent.
func TestRepairIdentities_Selection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	a, err := st.GetOrCreateSource("gmail", "frank@example.com") // email, no identity -> repaired
	require.NoError(err)
	b, err := st.GetOrCreateSource("gmail", "alice@example.com") // already confirmed -> skipped
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(b.ID, "alice@example.com", "preexisting"))
	c, err := st.GetOrCreateSource("gmail", "notanemail") // non-email identifier -> skipped
	require.NoError(err)
	d, err := st.GetOrCreateSource("imap", "bob@example.com") // wrong type -> skipped
	require.NoError(err)
	e, err := st.GetOrCreateSource("gmail", "https://user@example.com/mail") // URL, not an email -> skipped
	require.NoError(err)

	var buf bytes.Buffer
	n, err := repairIdentities(t.Context(), st, "gmail", "", &buf)
	require.NoError(err)
	assert.Equal(1, n, "only the valid gmail address without an identity is repaired")
	assert.Contains(buf.String(), "Activity and contact state catch up at the daemon's next projection, or right away with msgvault activity build.")

	ai, err := st.ListAccountIdentities(a.ID)
	require.NoError(err)
	assert.Len(ai, 1, "frank gets his own address confirmed")

	ci, err := st.ListAccountIdentities(c.ID)
	require.NoError(err)
	assert.Empty(ci, "non-email identifier is left alone")

	di, err := st.ListAccountIdentities(d.ID)
	require.NoError(err)
	assert.Empty(di, "imap source untouched when repairing gmail")

	ei, err := st.ListAccountIdentities(e.ID)
	require.NoError(err)
	assert.Empty(ei, "URL identifier is not accepted as an email address")

	bi, err := st.ListAccountIdentities(b.ID)
	require.NoError(err)
	assert.Len(bi, 1, "already-confirmed source is not doubled")

	// Idempotent: a second run repairs nothing (all now confirmed).
	buf.Reset()
	n2, err := repairIdentities(t.Context(), st, "gmail", "", &buf)
	require.NoError(err)
	assert.Zero(n2, "re-running repairs nothing once identities are confirmed")
	assert.NotContains(buf.String(), "Activity and contact state")
}

// The onlyIdentifier argument scopes the repair to a single account.
func TestRepairIdentities_SingleTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	a, err := st.GetOrCreateSource("gmail", "frank@example.com")
	require.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "carol@example.com")
	require.NoError(err)

	var buf bytes.Buffer
	n, err := repairIdentities(t.Context(), st, "gmail", "FRANK@example.com", &buf) // case-insensitive
	require.NoError(err)
	assert.Equal(1, n)

	ai, err := st.ListAccountIdentities(a.ID)
	require.NoError(err)
	assert.Len(ai, 1, "only the targeted account is confirmed")
}

func TestRepairIdentities_ReportsSkippedEmailSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	imapSource := newRepairIdentityIMAPSource(t, st)
	mboxSource, err := st.GetOrCreateSource("mbox", "boss@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(mboxSource.ID, "alias@example.com", "preexisting"))
	confirmed, err := st.GetOrCreateSource("imap", "confirmed@example.com")
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(confirmed.ID, "confirmed@example.com"))
	require.NoError(st.AddAccountIdentity(confirmed.ID, "confirmed@example.com", "preexisting"))
	granolaSource, err := st.GetOrCreateSource("granola", "meeting@example.com")
	require.NoError(err)

	var buf bytes.Buffer
	repaired, err := repairIdentities(t.Context(), st, "gmail", "", &buf)
	require.NoError(err)
	assert.Zero(repaired)
	assert.Equal("Skipped 1 imap source(s) with an unconfirmed account address; re-run with --type imap.\nSkipped 1 mbox source(s) with an unconfirmed account address; re-run with --type mbox.\n", buf.String())
	for _, source := range []*store.Source{imapSource, granolaSource} {
		identities, err := st.ListAccountIdentities(source.ID)
		require.NoError(err)
		assert.Empty(identities)
	}

	identities, err := st.ListAccountIdentities(mboxSource.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("alias@example.com", identities[0].Address)

	buf.Reset()
	repaired, err = repairIdentities(t.Context(), st, "gmail", "owner@example.com", &buf)
	require.NoError(err)
	assert.Zero(repaired)
	assert.Equal("Skipped 1 imap source(s) with an unconfirmed account address; re-run with --type imap and the same identifier.\n", buf.String())
}

func TestRepairIdentities_IMAPUsesConfiguredUsername(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newRepairIdentityIMAPSource(t, st)

	var buf bytes.Buffer
	repaired, err := repairIdentities(t.Context(), st, "imap", "", &buf)
	require.NoError(err)
	assert.Equal(1, repaired)

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("owner@example.com", identities[0].Address)
}

func TestRepairIdentities_IMAPTargetMatchesConfiguredUsername(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source := newRepairIdentityIMAPSource(t, st)

	var buf bytes.Buffer
	repaired, err := repairIdentities(t.Context(), st, "imap", "OWNER@example.com", &buf)
	require.NoError(err)
	assert.Equal(1, repaired)

	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("owner@example.com", identities[0].Address)
}

func TestRepairIdentities_SkipsOnlyMatchingIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	aliasOnly, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(aliasOnly.ID, "alias@example.com", "preexisting"))
	matching, err := st.GetOrCreateSource("gmail", "confirmed@example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(matching.ID, "CONFIRMED@EXAMPLE.COM", "preexisting"))

	var buf bytes.Buffer
	repaired, err := repairIdentities(t.Context(), st, "gmail", "", &buf)
	require.NoError(err)
	assert.Equal(1, repaired, "only the source missing its own address is repaired")

	aliasIdentities, err := st.ListAccountIdentities(aliasOnly.ID)
	require.NoError(err)
	assert.Len(aliasIdentities, 2, "an unrelated alias must not suppress the account address")
	matchingIdentities, err := st.ListAccountIdentities(matching.ID)
	require.NoError(err)
	assert.Len(matchingIdentities, 1, "case-equivalent account address is already confirmed")
}

func TestRepairIdentities_ReturnsAddFailuresAfterRemainingSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	failed, err := st.GetOrCreateSource("gmail", "fail@example.com")
	require.NoError(err)
	succeeded, err := st.GetOrCreateSource("gmail", "succeed@example.com")
	require.NoError(err)
	installFailingRepairIdentityTrigger(t, st)

	var buf bytes.Buffer
	repaired, err := repairIdentities(t.Context(), st, "gmail", "", &buf)
	require.Error(err)
	require.ErrorContains(err, "fail@example.com")
	require.ErrorContains(err, "forced identity failure")
	assert.Equal(1, repaired, "the later source is still repaired")
	assert.Contains(buf.String(), "Activity and contact state catch up at the daemon's next projection, or right away with msgvault activity build.")

	failedIdentities, listErr := st.ListAccountIdentities(failed.ID)
	require.NoError(listErr)
	assert.Empty(failedIdentities)
	succeededIdentities, listErr := st.ListAccountIdentities(succeeded.ID)
	require.NoError(listErr)
	assert.Len(succeededIdentities, 1)
}

func TestRepairIdentities_CancellationStopsBeforeNextSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	first, err := st.GetOrCreateSource("gmail", "a@example.com")
	require.NoError(err)
	second, err := st.GetOrCreateSource("gmail", "b@example.com")
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		var firstByte [1]byte
		_, _ = reader.Read(firstByte[:])
		cancel()
		_ = reader.CloseWithError(context.Canceled)
	}()

	repaired, err := repairIdentities(ctx, st, "gmail", "", writer)
	_ = writer.Close()
	<-readDone
	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, repaired, "the completed source remains reported")

	firstIdentities, listErr := st.ListAccountIdentities(first.ID)
	require.NoError(listErr)
	assert.Len(firstIdentities, 1)
	secondIdentities, listErr := st.ListAccountIdentities(second.ID)
	require.NoError(listErr)
	assert.Empty(secondIdentities, "cancellation prevents the next source repair")
}

func installFailingRepairIdentityTrigger(t *testing.T, st *store.Store) {
	t.Helper()

	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`
			CREATE FUNCTION fail_repair_identity() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.address = 'fail@example.com' THEN
					RAISE EXCEPTION 'forced identity failure';
				END IF;
				RETURN NEW;
			END
			$$;

			CREATE TRIGGER fail_repair_identity
			BEFORE INSERT ON account_identities
			FOR EACH ROW EXECUTE FUNCTION fail_repair_identity()
		`)
	} else {
		_, err = st.DB().Exec(`
			CREATE TRIGGER fail_repair_identity
			BEFORE INSERT ON account_identities
			WHEN NEW.address = 'fail@example.com'
			BEGIN
				SELECT RAISE(FAIL, 'forced identity failure');
			END`)
	}
	require.NoError(t, err)
}

func newRepairIdentityIMAPSource(t *testing.T, st *store.Store) *store.Source {
	t.Helper()

	imapCfg := &imapclient.Config{
		Host: "mail.example.com", Port: 993, TLS: true, Username: "owner@example.com",
	}
	source, err := st.GetOrCreateSource("imap", imapCfg.Identifier())
	require.NoError(t, err)
	configJSON, err := imapCfg.ToJSON()
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(source.ID, configJSON))
	return source
}
