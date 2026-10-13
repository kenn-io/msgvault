package carddav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type publicationAdmissionService interface {
	PublishReviewedPersonAuthorizedWithAdmission(ctx context.Context, personID int64, token string, authorize store.PersonEditAuthorizer, admission func(context.Context, store.CardDAVPublication) error) error
}

func TestPublicationAdmissionCommitsBeforeProviderDispatch(t *testing.T) {
	for _, mode := range []string{"allowed", "sql_failure", "nil_admission", "mutated_snapshot", "revoked_after_admission"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := &mutationFixture{}
			var scoped, revoked atomic.Bool
			var puts, gets, admissions atomic.Int32
			var admittedIntent atomic.Value
			var previewBody []byte
			var st *store.Store
			baseHandler := fixture.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scoped.Load() {
					intent, _ := admittedIntent.Load().(string)
					var count int
					err := st.DB().QueryRowContext(r.Context(), st.Rebind("SELECT COUNT(*) FROM publication_admission_probe WHERE intent = ?"), intent).Scan(&count)
					assert.NoError(err)
					assert.Equal(1, count, "provider work requires an independently committed admission")
					switch r.Method {
					case http.MethodPut:
						puts.Add(1)
						body, err := io.ReadAll(r.Body)
						assert.NoError(err)
						assert.Equal(previewBody, body, "admission cannot change the reviewed provider bytes")
						r.Body = io.NopCloser(bytes.NewReader(body))
					case http.MethodGet:
						gets.Add(1)
					default:
						assert.Fail("publication may only update and observe its mapped resource")
					}
				}
				baseHandler(w, r)
			}))
			t.Cleanup(server.Close)
			service, nativeStore, personID, _ := seededMutationServiceForServer(t, server)
			st = nativeStore
			backend, ok := any(service).(publicationAdmissionService)
			require.True(ok, "receipt admission must run after native preparation and before any HTTP")
			_, err := st.DB().ExecContext(t.Context(), "CREATE TABLE publication_admission_probe (intent TEXT PRIMARY KEY)")
			require.NoError(err)
			t.Cleanup(func() {
				_, err := st.DB().ExecContext(context.Background(), "DROP TABLE publication_admission_probe")
				assert.NoError(err)
			})
			require.NoError(service.PublishPerson(t.Context(), personID))
			appendInferenceReviewNote(t, st, personID, "Synthetic admitted contact correction")
			denied := errors.New("synthetic publication authority revoked")
			authorize := func(_ context.Context, _ *store.IdentityGrantSelection) error {
				if revoked.Load() {
					return denied
				}
				return nil
			}
			preview, err := service.PreviewPublicationAuthorized(t.Context(), personID, authorize)
			require.NoError(err)
			previewBody = []byte(preview.VCard)
			if mode == "sql_failure" {
				_, err := st.DB().ExecContext(t.Context(), "INSERT INTO publication_admission_probe VALUES ('synthetic-conflict')")
				require.NoError(err)
			}
			var admissionError error
			admission := func(ctx context.Context, snapshot store.CardDAVPublication) error {
				admissions.Add(1)
				current, err := st.GetCardDAVPublicationContext(ctx, personID)
				if err != nil {
					return err
				}
				assert.NotEmpty(snapshot.PendingIntentID)
				assert.Equal(current.PendingIntentID, snapshot.PendingIntentID, "native preparation must already be visible outside its transaction")
				intent := snapshot.PendingIntentID
				if mode == "sql_failure" {
					intent = "synthetic-conflict"
				}
				_, admissionError = st.DB().ExecContext(ctx, st.Rebind("INSERT INTO publication_admission_probe VALUES (?)"), intent)
				if admissionError != nil {
					return admissionError
				}
				admittedIntent.Store(snapshot.PendingIntentID)
				if mode == "mutated_snapshot" {
					snapshot.OutgoingBody[0] = '!'
					snapshot.Href = "https://unselected.example.test/contact.vcf"
					snapshot.PendingIntentID = "synthetic-replaced-intent"
					if snapshot.ApprovedBodySHA256 != nil {
						*snapshot.ApprovedBodySHA256 = "synthetic-tampered-digest"
					}
					if len(snapshot.OutgoingEnvelopeMetadata) > 0 {
						snapshot.OutgoingEnvelopeMetadata[0] = '!'
					}
				}
				if mode == "revoked_after_admission" {
					revoked.Store(true)
				}
				return nil
			}
			if mode == "nil_admission" {
				admission = nil
			}
			scoped.Store(true)
			err = backend.PublishReviewedPersonAuthorizedWithAdmission(t.Context(), personID, preview.ApprovalToken, authorize, admission)
			switch mode {
			case "allowed", "mutated_snapshot":
				require.NoError(err)
				assert.Equal(int32(1), admissions.Load())
				assert.Equal(int32(1), puts.Load())
				assert.Equal(int32(1), gets.Load())
				settled, err := st.GetCardDAVPublicationContext(t.Context(), personID)
				require.NoError(err)
				assert.Empty(settled.PendingIntentID)
			case "nil_admission":
				require.ErrorIs(err, store.ErrCardDAVInvalidPlan)
				assert.Zero(admissions.Load())
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
			default:
				if mode == "sql_failure" {
					require.Error(admissionError)
					require.ErrorIs(err, admissionError)
				} else {
					require.ErrorIs(err, denied)
				}
				assert.Equal(int32(1), admissions.Load())
				assert.Zero(puts.Load())
				assert.Zero(gets.Load())
				pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
				require.NoError(err)
				assert.NotEmpty(pending.PendingIntentID)
				assert.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
			}
		})
	}
}
