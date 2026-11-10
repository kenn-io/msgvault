package meta

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestApplyInstagramCommentsReadsAndVerifiesNodes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	owner := "200"
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100":
			_, _ = fmt.Fprint(w, `{"id":"100","instagram_business_account":{"id":"200","username":"example_business"}}`)
		case "/v26.0/300":
			_, _ = fmt.Fprintf(w, `{"id":"300","caption":"Synthetic caption","timestamp":"2000-01-02T03:04:05+0000","owner":{"id":%q}}`, owner)
		case "/v26.0/500":
			_, _ = fmt.Fprint(w, `{"id":"500","text":"top comment","timestamp":"2000-01-02T04:00:00+0000","from":{"id":"700","username":"example_visitor"},"media":{"id":"300"}}`)
		case "/v26.0/501":
			_, _ = fmt.Fprint(w, `{"id":"501","text":"read from Graph","timestamp":"2000-01-02T05:00:00+0000","from":{"id":"701","username":"example_friend"},"parent_id":"500","media":{"id":"300"}}`)
		default:
			assert.Fail("unexpected Graph read", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	src, err := NewImporter(st, c).RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)

	owner = "999"
	err = ApplyInstagramComments(t.Context(), st, c, "9", "100", "200", feedValues(`{"id":"501","media":{"id":"300"}}`))
	require.ErrorIs(err, ErrInvalidChange, "media owned by another account is refused")
	ids, err := st.MessageExistsBatch(src.ID, []string{"300", "500", "501"})
	require.NoError(err)
	assert.Empty(ids)

	owner = "200"
	require.ErrorIs(ApplyInstagramComments(t.Context(), st, c, "9", "100", "200", feedValues(`{"id":"501","media":{"id":"301"}}`)), ErrInvalidChange)
	// The delivery's text is ignored; the archived text is the node read from Graph.
	require.NoError(ApplyInstagramComments(t.Context(), st, c, "9", "100", "200", feedValues(`{"id":"501","text":"delivery text","media":{"id":"300"}}`)))
	media, top, reply := message(t, st, src.ID, "300"), message(t, st, src.ID, "500"), message(t, st, src.ID, "501")
	assert.Equal(media.ID, top.ReplyToMessageID.Int64)
	assert.Equal(top.ID, reply.ReplyToMessageID.Int64)
	assert.Equal(InstagramSourceType, reply.MessageType)
	body, err := st.GetMessageBodyText(t.Context(), reply.ID)
	require.NoError(err)
	assert.Equal("read from Graph", body)
}
