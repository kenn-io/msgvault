package gmail

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Mapping validation must obtain existing native tags, never create labels or
// treat protected Inbox/read labels as GTD tags.
func TestInboxGmailTagCatalogExistingLabels(t *testing.T) {
	for _, tc := range []struct {
		name, labels, account string
		foreignSource         bool
		want                  []emailtags.Tag
		wantErr               error
	}{
		{name: "existing user labels", account: "owner@example.com", labels: `{"labels":[{"id":"INBOX","name":"Inbox","type":"system"},{"id":"UNREAD","name":"Unread","type":"system"},{"id":"LabelTodo","name":"Todo","type":"user"},{"id":"LabelWatch","name":"Watch","type":"user"}]}`, want: []emailtags.Tag{{ID: "LabelTodo", Name: "Todo"}, {ID: "LabelWatch", Name: "Watch"}}},
		{name: "known empty", account: "owner@example.com", labels: `{"labels":[]}`, want: []emailtags.Tag{}},
		{name: "missing evidence", account: "owner@example.com", labels: `{}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "duplicate identity", account: "owner@example.com", labels: `{"labels":[{"id":"LabelTodo","name":"Todo","type":"user"},{"id":"LabelTodo","name":"Watch","type":"user"}]}`, wantErr: inboxcontrol.ErrUnavailable},
		{name: "foreign provider account", account: "foreign@example.test", labels: `{"labels":[]}`, wantErr: inboxcontrol.ErrDenied},
		{name: "foreign archive source", account: "owner@example.com", foreignSource: true, labels: `{"labels":[]}`, wantErr: inboxcontrol.ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			reads, writes := 0, 0
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					http.Error(w, "unexpected mutation", http.StatusMethodNotAllowed)
					return
				}
				reads++
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					_, _ = w.Write([]byte(`{"emailAddress":"` + tc.account + `"}`))
				case "/gmail/v1/users/me/labels":
					_, _ = w.Write([]byte(tc.labels))
				default:
					assert.Fail(t, "unexpected catalog request", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			source := inboxGmailSource()
			provider := NewInboxProvider(client, source)
			catalog, ok := any(provider).(interface {
				TagCatalog(ctx context.Context, source inboxcontrol.SourceIdentity) ([]emailtags.Tag, error)
			})
			requirements.True(ok, "native provider must expose existing GTD tag identities")
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
			assertions.Zero(writes, "catalog lookup must not provision or change provider state")
			if tc.foreignSource {
				assertions.Zero(reads, "foreign source must be rejected before provider access")
			}
		})
	}
}
