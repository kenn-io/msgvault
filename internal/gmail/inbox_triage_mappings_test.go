package gmail

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type triageMappingService interface {
	UpdateTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity, entries map[string]string, expectedRevision int64, principal inboxcontrol.Principal, acquireWrite func(context.Context) (func(), error)) (int64, error)
}

func TestInboxGmailOwnerMappingValidatesExistingLabels(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		entries                              map[string]string
		delegate, denied, revoked, noCatalog bool
		want                                 error
	}{
		{name: "existing native tag", entries: map[string]string{"todo": "Label_1"}},
		{name: "unknown native tag", entries: map[string]string{"todo": "NewLabel"}, want: inboxcontrol.ErrDenied},
		{name: "protected native marker", entries: map[string]string{"todo": "INBOX"}, want: inboxcontrol.ErrDenied},
		{name: "delegate denied", entries: map[string]string{"todo": "Label_1"}, delegate: true, want: inboxcontrol.ErrDenied},
		{name: "source permission denied", entries: map[string]string{"todo": "Label_1"}, denied: true, want: inboxcontrol.ErrDenied},
		{name: "revoked while acquiring gate", entries: map[string]string{"todo": "Label_1"}, revoked: true, want: inboxcontrol.ErrDenied},
		{name: "provider catalog absent", entries: map[string]string{"todo": "Label_1"}, noCatalog: true, want: inboxcontrol.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f := storetest.New(t)
			source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
			var reads, writes atomic.Int64
			gateCalls, releases := 0, 0
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				reads.Add(1)
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					_, err := fmt.Fprintf(w, `{"emailAddress":%q}`, f.Source.Identifier)
					assert.NoError(t, err)
				case "/gmail/v1/users/me/labels":
					_, err := fmt.Fprint(w, `{"labels":[{"id":"INBOX","name":"Inbox","type":"system"},{"id":"Label_1","name":"Todo","type":"user"}]}`)
					assert.NoError(t, err)
				default:
					assert.Fail(t, "unexpected native lookup", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			allowed := !tc.denied
			service := &inboxcontrol.Service{Ledger: f.Store,
				Authorize: func(_ context.Context, _ inboxcontrol.Principal, r inboxcontrol.Request) error {
					assert.Equal(t, inboxcontrol.OpGetCapabilities, r.Operation)
					require.NotNil(t, r.Source)
					assert.Equal(t, source, *r.Source)
					if !allowed {
						return inboxcontrol.ErrDenied
					}
					return nil
				},
				AcquireSource: func(ctx context.Context, id int64) (func(), error) {
					execution, err := f.Store.AcquireSyncExecutionContext(ctx, id)
					if err != nil {
						return nil, err
					}
					return func() { assert.NoError(t, execution.Release()) }, nil
				},
				Resolve: func(_ context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
					require.NotNil(t, r.Source)
					assert.Equal(t, source, *r.Source)
					provider := NewInboxProvider(client, source)
					if tc.noCatalog {
						return struct{ inboxcontrol.Provider }{provider}, nil
					}
					return provider, nil
				},
			}
			updater, ok := any(service).(triageMappingService)
			requirements.True(ok, "owner mapping update must validate native catalog under source/write gates")
			principal := inboxcontrol.Principal{ID: "owner-fixture", Owner: !tc.delegate}
			gate := func(context.Context) (func(), error) {
				gateCalls++
				if tc.revoked {
					allowed = false
				}
				return func() { releases++ }, nil
			}
			revision, err := updater.UpdateTriageMappings(t.Context(), source, tc.entries, 0, principal, gate)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assertions.Zero(revision)
			} else {
				requirements.NoError(err)
				assertions.Equal(int64(1), revision)
			}
			assertions.Zero(writes.Load(), "mapping updates cannot provision labels or mutate Inbox/read state")
			assertions.Equal(gateCalls, releases, "failed and successful updates must release write gate")
			if tc.delegate || tc.denied {
				assertions.Zero(gateCalls)
			}
			if tc.delegate || tc.denied || tc.revoked || tc.noCatalog {
				assertions.Zero(reads.Load())
			}
			reader, ok := any(f.Store).(interface {
				InboxTriageMappings(ctx context.Context, source inboxcontrol.SourceIdentity) (map[string]string, int64, error)
			})
			requirements.True(ok)
			stored, storedRevision, err := reader.InboxTriageMappings(t.Context(), source)
			requirements.NoError(err)
			if tc.want != nil {
				assertions.Empty(stored)
				assertions.Zero(storedRevision)
			} else {
				assertions.Equal(tc.entries, stored)
				assertions.Equal(int64(1), storedRevision)
			}
		})
	}
}
