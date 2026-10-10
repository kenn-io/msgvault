package msmail

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestMicrosoftInboxTagCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, body, secondPage        string
		status                        int
		foreignSource, foreignAccount bool
		want                          []emailtags.Tag
		wantErr                       error
	}{
		{name: "native category names", body: `{"value":[{"id":"master-2","displayName":"Watch","color":"preset1"},{"id":"master-1","displayName":"Todo","color":"preset0"}]}`, want: []emailtags.Tag{{ID: "Todo", Name: "Todo"}, {ID: "Watch", Name: "Watch"}}},
		{name: "known empty", body: `{"value":[]}`, want: []emailtags.Tag{}},
		{name: "paged catalog", body: `{"value":[{"id":"one","displayName":"Todo"}],"@odata.nextLink":"/me/outlook/masterCategories?$skiptoken=2"}`, secondPage: `{"value":[{"id":"two","displayName":"Watch"}]}`, want: []emailtags.Tag{{ID: "Todo", Name: "Todo"}, {ID: "Watch", Name: "Watch"}}},
		{name: "foreign page", body: `{"value":[],"@odata.nextLink":"https://other.example.com/me/outlook/masterCategories"}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "unrelated page", body: `{"value":[],"@odata.nextLink":"/me/messages"}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "page loop", body: `{"value":[],"@odata.nextLink":"/me/outlook/masterCategories?$select=id,displayName"}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "missing collection", body: `{}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "duplicate category", body: `{"value":[{"id":"one","displayName":"Todo"},{"id":"two","displayName":"Todo"}]}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "missing category name", body: `{"value":[{"id":"one"}]}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "permission absent", status: http.StatusForbidden, wantErr: inboxcontrol.ErrDenied},
		{name: "foreign source", foreignSource: true, wantErr: inboxcontrol.ErrDenied},
		{name: "foreign account", foreignAccount: true, wantErr: inboxcontrol.ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			var calls, catalogs, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				assert.Equal(t, http.MethodGet, r.Method)
				switch r.URL.Path {
				case "/me":
					account := "mailbox@example.com"
					if tc.foreignAccount {
						account = "other@example.com"
					}
					_, err := fmt.Fprintf(w, `{"mail":%q,"userPrincipalName":%q}`, account, account)
					assert.NoError(t, err)
				case "/me/outlook/masterCategories":
					catalogs.Add(1)
					body := tc.body
					if r.URL.Query().Get("$skiptoken") == "2" {
						body = tc.secondPage
					} else {
						assert.Equal(t, "id,displayName", r.URL.Query().Get("$select"))
					}
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					_, err := fmt.Fprint(w, body)
					assert.NoError(t, err)
				default:
					assert.Fail(t, "unexpected Graph request", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			t.Cleanup(server.Close)
			source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "msmail", SourceIdentifier: "mailbox@example.com", AccountID: "mailbox@example.com"}
			provider := NewInboxProvider(NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000), source)
			catalog, ok := any(provider).(inboxcontrol.TagCatalogProvider)
			requirements.True(ok, "Microsoft must validate existing native category names before mapping")
			if tc.foreignSource {
				source.SourceID++
			}
			tags, err := catalog.TagCatalog(t.Context(), source)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				requirements.NoError(err)
				assertions.Equal(tc.want, tags)
			}
			assertions.Zero(writes.Load())
			if tc.foreignSource {
				assertions.Zero(calls.Load())
			}
			if tc.foreignAccount {
				assertions.Zero(catalogs.Load())
			}
		})
	}
}
