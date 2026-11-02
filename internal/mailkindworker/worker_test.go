package mailkindworker_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/mailkindworker"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const acceptedMailKind = `{"model":"jev-1.13.0","answers":{"transactional":{"type":"noul","noul":0.95},"kind":{"type":"choice","choice":"receipt","confidence":0.2,"probabilities":{"order":0,"shipment":0,"receipt":0.9,"invoice":0.1,"flight":0,"lodging":0,"reservation":0,"event_ticket":0,"subscription":0,"other":0}}}}`

func mailKindFixture(t *testing.T, grant bool) (*store.Store, mailkind.Config, int64, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, id, err := testutil.CreateIndexedSourceMessage(st, "fixture-owner@example.test", "mail-kind-1", "Purchase summary", "Your purchase has been paid.")
	require.NoError(t, err)
	cfg := mailkind.Config{Enabled: true, CredentialEnv: "MSGVAULT_TEST_MAIL_KIND_WORKER_KEY", RetentionDeclaration: "synthetic provider policy"}
	cfg.ApplyDefaults()
	t.Setenv(cfg.CredentialEnv, "synthetic-key")
	if grant {
		disclosure, err := cfg.Disclosure()
		require.NoError(t, err)
		_, _, err = st.GrantMailKindConsentContext(t.Context(), disclosure, "fixture", nil)
		require.NoError(t, err)
	}
	return st, cfg, source.ID, id
}

func TestMailKindWorkerPersistsOnlyLocalKind(t *testing.T) {
	st, cfg, sourceID, id := mailKindFixture(t, true)
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var request struct {
			State mailkind.Packet `json:"state"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "Your purchase has been paid.", request.State.Excerpt)
		_, _ = w.Write([]byte(acceptedMailKind))
	}))
	defer provider.Close()
	worker := mailkindworker.Worker{Store: st, Config: cfg, Endpoint: provider.URL, HTTPClient: provider.Client()}
	counts, err := worker.Run(t.Context(), 1, []int64{id})
	require.NoError(t, err)
	assert.Equal(t, mailkindworker.Counts{Classified: 1, Labeled: 1, Requests: 1}, counts)
	kind, err := st.MailKindLabelContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "receipt", kind)
	var labelType string
	var labelSource sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT source_id, label_type FROM labels WHERE name = 'mv:kind/receipt'`).Scan(&labelSource, &labelType))
	assert.False(t, labelSource.Valid)
	assert.Equal(t, "auto", labelType)
	// The existing search syntax sees the local label without another index.
	messages, _, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("label:mv:kind/receipt"), 0, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, id, messages[0].ID)
	counts, err = worker.Run(t.Context(), 1, []int64{id})
	require.NoError(t, err)
	assert.Zero(t, counts.Requests)
	assert.EqualValues(t, 1, requests.Load())
	// Even a direct concurrent replay cannot create a second label link.
	_, token, err := st.MailKindInputContext(t.Context(), id)
	require.NoError(t, err)
	disclosure, err := cfg.Disclosure()
	require.NoError(t, err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	changed, err := st.ApplyMailKindLabelContext(t.Context(), fingerprint, id, token, "invoice", nil)
	require.NoError(t, err)
	assert.False(t, changed)
	// Source restrictions still apply to candidate selection and label readout.
	denied := sourcescope.WithSourceIDs(t.Context(), []int64{sourceID + 10000})
	ids, err := st.ListMailKindCandidatesContext(denied, 1, []int64{id})
	require.NoError(t, err)
	assert.Empty(t, ids)
	kind, err = st.MailKindLabelContext(denied, id)
	require.NoError(t, err)
	assert.Empty(t, kind)
}

func TestMailKindWorkerPreflightSendsNothing(t *testing.T) {
	for _, mode := range []string{"disabled", "no consent", "no key", "changed disclosure"} {
		t.Run(mode, func(t *testing.T) {
			st, cfg, _, id := mailKindFixture(t, mode != "no consent")
			var requests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer provider.Close()
			switch mode {
			case "disabled":
				cfg.Enabled = false
			case "no key":
				t.Setenv(cfg.CredentialEnv, "")
			case "changed disclosure":
				cfg.RetentionDeclaration += " changed"
			default:
			}
			_, err := (mailkindworker.Worker{Store: st, Config: cfg, Endpoint: provider.URL}).Run(t.Context(), 1, []int64{id})
			require.Error(t, err)
			assert.Zero(t, requests.Load())
		})
	}
}

func TestMailKindWorkerSkipsDeterministicAndCredentials(t *testing.T) {
	for _, message := range []struct{ subject, text, html string }{
		{"Order notice", "", `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"ORD-B"}</script>`},
		{"Your verification code", "Your sign-in code is 654321.", ""},
	} {
		st, cfg, _, id := mailKindFixture(t, true)
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), message.subject, id)
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: message.text, Valid: true}, sql.NullString{String: message.html, Valid: true}))
		var requests atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
		counts, err := (mailkindworker.Worker{Store: st, Config: cfg, Endpoint: provider.URL}).Run(t.Context(), 1, []int64{id})
		provider.Close()
		require.NoError(t, err)
		assert.Equal(t, 1, counts.Undecided)
		assert.Zero(t, requests.Load())
	}
}

func TestMailKindWorkerAutomaticScanAdvancesWithoutRepeatingUndecidedMessages(t *testing.T) {
	st, cfg, _, olderID := mailKindFixture(t, true)
	_, newerID, err := testutil.CreateIndexedSourceMessage(
		st, "fixture-owner@example.test", "mail-kind-verification", "Your verification code", "Your sign-in code is 654321.")
	require.NoError(t, err)
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(acceptedMailKind))
	}))
	defer provider.Close()
	worker := mailkindworker.Worker{Store: st, Config: cfg, Endpoint: provider.URL, HTTPClient: provider.Client()}

	counts, err := worker.Run(t.Context(), 1, nil)
	require.NoError(t, err)
	assert.Equal(t, mailkindworker.Counts{Undecided: 1}, counts)
	assert.Zero(t, requests.Load(), "the newest deterministic message should not reach the provider")

	counts, err = worker.Run(t.Context(), 1, nil)
	require.NoError(t, err)
	assert.Equal(t, mailkindworker.Counts{Classified: 1, Labeled: 1, Requests: 1}, counts)
	kind, err := st.MailKindLabelContext(t.Context(), olderID)
	require.NoError(t, err)
	assert.Equal(t, "receipt", kind)

	counts, err = worker.Run(t.Context(), 1, nil)
	require.NoError(t, err)
	assert.Zero(t, counts)
	assert.EqualValues(t, 1, requests.Load(), "unchanged undecided messages should not be sent repeatedly")

	// A changed existing message remains explicitly retryable after the
	// automatic cursor has completed its initial archive pass.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET subject = ? WHERE id = ?`), "Your purchase receipt", newerID)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(newerID, sql.NullString{String: "Your purchase has been paid.", Valid: true}, sql.NullString{}))
	counts, err = worker.Run(t.Context(), 1, []int64{newerID})
	require.NoError(t, err)
	assert.Equal(t, mailkindworker.Counts{Classified: 1, Labeled: 1, Requests: 1}, counts)
	assert.EqualValues(t, 2, requests.Load())

	_, newestID, err := testutil.CreateIndexedSourceMessage(
		st, "fixture-owner@example.test", "mail-kind-new-arrival", "Order receipt", "Your order is confirmed.")
	require.NoError(t, err)
	counts, err = worker.Run(t.Context(), 1, nil)
	require.NoError(t, err)
	assert.Equal(t, mailkindworker.Counts{Classified: 1, Labeled: 1, Requests: 1}, counts)
	kind, err = st.MailKindLabelContext(t.Context(), newestID)
	require.NoError(t, err)
	assert.Equal(t, "receipt", kind)
	assert.EqualValues(t, 3, requests.Load())
}

func TestMailKindWorkerRejectsChangedEvidenceBeforeLabel(t *testing.T) {
	st, cfg, _, id := mailKindFixture(t, true)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !assert.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: "Changed purchase body", Valid: true}, sql.NullString{})) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(acceptedMailKind))
	}))
	defer provider.Close()
	counts, err := (mailkindworker.Worker{Store: st, Config: cfg, Endpoint: provider.URL}).Run(t.Context(), 1, []int64{id})
	require.NoError(t, err)
	assert.Equal(t, mailkindworker.Counts{Classified: 1, Undecided: 1, Requests: 1}, counts)
	kind, err := st.MailKindLabelContext(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, kind)
}

type revokeAfterRequest struct {
	*store.Store

	fingerprint string
}

func (s revokeAfterRequest) MailKindConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error) {
	allowed, err := s.Store.MailKindConsentEgressContext(ctx, fingerprint, dispatch)
	if err == nil && allowed {
		_, err = s.RevokeMailKindConsentContext(ctx, s.fingerprint, "fixture", nil)
	}
	return allowed, err
}

func TestMailKindWorkerRevocationBlocksLabel(t *testing.T) {
	st, cfg, _, id := mailKindFixture(t, true)
	disclosure, err := cfg.Disclosure()
	require.NoError(t, err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(acceptedMailKind)) }))
	defer provider.Close()
	counts, err := (mailkindworker.Worker{Store: revokeAfterRequest{st, fingerprint}, Config: cfg, Endpoint: provider.URL}).Run(t.Context(), 1, []int64{id})
	require.ErrorIs(t, err, mailkindworker.ErrConsentRequired)
	assert.Equal(t, 1, counts.Requests)
	assert.Zero(t, counts.Labeled)
}

func TestMailKindWorkerThresholdsAndMalformedReply(t *testing.T) {
	for name, response := range map[string]string{
		"transactional below floor": strings.Replace(acceptedMailKind, `"noul":0.95`, `"noul":0.89`, 1),
		"choice below floor":        strings.Replace(strings.Replace(acceptedMailKind, `"receipt":0.9`, `"receipt":0.79`, 1), `"invoice":0.1`, `"invoice":0.21`, 1),
		"malformed":                 `{"model":"jev-1.13.0","answers":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			st, cfg, _, id := mailKindFixture(t, true)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) }))
			defer provider.Close()
			counts, err := (mailkindworker.Worker{Store: st, Config: cfg, Endpoint: provider.URL}).Run(t.Context(), 1, []int64{id})
			require.NoError(t, err)
			assert.Equal(t, 1, counts.Requests)
			assert.Zero(t, counts.Labeled)
			if name == "malformed" {
				assert.Equal(t, 1, counts.Errors)
			} else {
				assert.Equal(t, 1, counts.Undecided)
			}
			kind, err := st.MailKindLabelContext(t.Context(), id)
			require.NoError(t, err)
			assert.Empty(t, kind)
		})
	}
}
