package carddav

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOwnerRecoverySettlesOriginalPublicationReceipt(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite settlement failure trigger; native receipt SQL contracts also run on PostgreSQL")
	for _, mode := range []string{"verified", "remote_changed", "local_changed"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			fixture := &mutationFixture{}
			var scoped atomic.Bool
			var puts, gets atomic.Int32
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					if r.Method == http.MethodPut {
						puts.Add(1)
					}
					if r.Method == http.MethodGet {
						gets.Add(1)
					}
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, st, personID, book := seededMutationServiceForServer(t, server)
			require.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic owner receipt recovery")
			allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER synthetic_owner_receipt_failure BEFORE UPDATE ON carddav_publication_receipts BEGIN SELECT RAISE(ABORT,'synthetic owner receipt failure'); END`)
			require.NoError(err)
			scoped.Store(true)
			first, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-owner-recovery", allow)
			require.ErrorContains(err, "synthetic owner receipt failure")
			require.NotNil(first)
			require.Equal("dispatching", first.State)
			beforeRecovery, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			require.NotEmpty(beforeRecovery.PendingIntentID)
			_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER synthetic_owner_receipt_failure`)
			require.NoError(err)
			if mode == "remote_changed" {
				person, err := st.GetPerson(personID)
				require.NoError(err)
				fixture.mu.Lock()
				fixture.body = []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Foreign Card\r\nEND:VCARD\r\n", person.VCardUID))
				fixture.etag = `"synthetic-foreign-etag"`
				fixture.mu.Unlock()
			} else if mode == "local_changed" {
				profile, err := st.GetPersonProfileContext(t.Context(), personID)
				require.NoError(err)
				_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{
					Names: &store.PersonNamePatch{Add: []store.PersonNameInput{{
						NameKind: store.PersonNameFormatted, Formatted: new("Synthetic Local Edit"),
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					}}},
				})
				require.NoError(err)
			}
			recovered, err := service.recoverPendingPublications(t.Context())
			var conflictID int64
			require.NoError(err, "owner recovery records mismatches as conflicts and lets sync continue")
			if mode != "verified" {
				conflict, err := st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
				require.NoError(err)
				assert.Positive(conflict.ID, "owner recovery moves a mismatched receipt intent into a reviewable conflict")
				conflictID = conflict.ID
			}
			assert.True(recovered[personID])
			pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			wantState := "verified"
			if mode != "verified" {
				assert.Empty(pending.PendingIntentID, "the conflicted owner intent must stop blocking synchronization")
				assert.Empty(pending.PendingOperation)
				conflict, err := st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
				require.NoError(err)
				assert.Equal(conflictID, conflict.ID)
				wantState = "dispatching"
			} else {
				assert.Empty(pending.PendingIntentID)
			}
			receipt, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-owner-recovery", allow)
			require.NoError(err)
			assert.Equal(first.ID, receipt.ID)
			assert.Equal(wantState, receipt.State, "owner reconciliation must preserve the original receipt history")
			if conflictID != 0 {
				assert.NotNil(receipt, "the historical receipt remains available while its conflict is reviewed")
			}
			assert.Equal(int32(1), puts.Load(), "owner recovery must not replay the provider write")
			assert.Equal(int32(2), gets.Load())
		})
	}
}

func TestScopedPublication412MovesToConflictBeforeSync(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	fixture := &mutationFixture{exactGet: true}
	var scoped atomic.Bool
	var puts atomic.Int32
	var href, hrefPath string
	baseHandler := fixture.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			if scoped.Load() {
				fixture.mu.Lock()
				fixture.body = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-person\r\nFN:Synthetic Concurrent Edit\r\nEND:VCARD\r\n")
				fixture.etag = `"synthetic-foreign-etag"`
				fixture.mu.Unlock()
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			baseHandler(w, r)
			return
		}
		if r.Method == "REPORT" {
			body := readRequestBody(t, r)
			if strings.Contains(body, "sync-collection") {
				writeDAVXML(t, w, syncResponse(changedResponse(hrefPath, `&quot;synthetic-foreign-etag&quot;`), "synthetic-sync-token"))
				return
			}
			if strings.Contains(body, "addressbook-multiget") {
				fixture.mu.Lock()
				remoteBody := strings.ReplaceAll(string(fixture.body), "\r\n", "&#13;\n")
				fixture.mu.Unlock()
				etag := strings.ReplaceAll(`"synthetic-foreign-etag"`, `"`, "&quot;")
				response := `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>` + hrefPath +
					`</D:href><D:propstat><D:prop><D:getetag>` + etag + `</D:getetag><C:address-data>` + remoteBody +
					`</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
				writeDAVXML(t, w, response)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		baseHandler(w, r)
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	var personID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), "synthetic-person", "Synthetic Person").Scan(&personID))
	require.NoError(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "Synthetic reviewed contact correction")
	allow := func(context.Context, *store.IdentityGrantSelection) error { return nil }
	preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, allow)
	require.NoError(err)

	href = book.CanonicalURL + "synthetic-person.vcf"
	hrefPath = mustParseURL(t, href).EscapedPath()
	scoped.Store(true)
	first, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-412-recovery", allow)
	require.ErrorIs(err, ErrScopedPublicationReconciliationRequired)
	require.NotNil(first)
	assert.Equal("dispatching", first.State)
	assert.Equal(int32(2), puts.Load(), "the scoped conditional update was attempted once")

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err, "owner recovery should create a conflict and let the address-book pull finish")
	assert.Equal(1, result.Books)
	assert.Equal(int32(2), puts.Load(), "sync must not retry the receipt-bound provider write")
	conflict, err := st.GetUnresolvedCardDAVConflictForMappingContext(t.Context(), book.ID, href)
	require.NoError(err)
	assert.Positive(conflict.ID, "the owner recovery leaves a reviewable conflict")
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(pending.PendingIntentID, "a conflict must stop blocking later syncs")
	assert.Empty(pending.PendingOperation)

	replayed, err := service.PublishReviewedPersonWithReceipt(t.Context(), personID, preview.ApprovalToken, "synthetic-owner", "synthetic-412-recovery", allow)
	require.NoError(err)
	require.NotNil(replayed)
	assert.Equal(first.ID, replayed.ID)
	assert.Equal("dispatching", replayed.State, "the receipt retains evidence that the original write outcome was ambiguous")
}
