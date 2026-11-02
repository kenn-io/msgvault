package meta

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockAuthorExactRequest(t *testing.T) {
	for _, block := range []bool{true, false} {
		t.Run(fmt.Sprint(block), func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				wantMethod := http.MethodDelete
				if block {
					wantMethod = http.MethodPost
				}
				assert.Equal(t, wantMethod, r.Method)
				assert.Equal(t, "/v26.0/100/blocked", r.URL.Path)
				assert.Empty(t, r.URL.RawQuery)
				data, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				form, err := url.ParseQuery(string(data))
				require.NoError(t, err)
				require.Len(t, form, 1)
				if block {
					var users []string
					require.NoError(t, json.Unmarshal([]byte(form.Get("user")), &users))
					assert.Equal(t, []string{"300"}, users)
					_, _ = fmt.Fprint(w, `{"300":true}`)
				} else {
					assert.Equal(t, "300", form.Get("user"))
					_, _ = fmt.Fprint(w, `{"success":true}`)
				}
			})
			var err error
			if block {
				err = c.BlockUser(t.Context(), "100", "300")
			} else {
				err = c.UnblockUser(t.Context(), "100", "300")
			}
			require.NoError(t, err)
		})
	}
}
func TestBlockRequiresConfirm(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	g.response = `{"300":true}`
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock})
	actionCode(t, err, "confirmation_mismatch")
	assert.Zero(t, g.count())
	got, err := actionRun(t, a, id, ActionBlock)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "300", rows[0].TargetAuthorID)
	_, err = actionRun(t, a, id, ActionBlock)
	require.NoError(t, err)
	assert.Equal(t, 1, g.count())
}
func TestBlockAuthorUnknownRefused(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	require.NoError(t, st.UpsertMessageRawWithFormat(id, []byte(`{"id":"100_200_300"}`), "facebook-comment-json"))
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock, DryRun: true})
	actionCode(t, err, "author_unknown")
	assert.Zero(t, g.count())
}
func TestBlockPageAuthorRefused(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	require.NoError(t, st.UpsertMessageRawWithFormat(id, []byte(`{"id":"100_200_300","from":{"id":"100","name":"Example Page"}}`), "facebook-comment-json"))
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock, DryRun: true})
	actionCode(t, err, "not_blockable")
	assert.Zero(t, g.count())
}
func TestBlockInstagramUnsupported(t *testing.T) {
	st, _, _, a, g := actionFixture(t)
	_, client := newInstagramImportFixture(t)
	imp := NewImporter(st, client)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(t, err)
	_, err = imp.ImportInstagram(t.Context(), "9", "100", "200", ImportOptions{})
	require.NoError(t, err)
	id := message(t, st, src.ID, "400").ID
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock, DryRun: true})
	actionCode(t, err, "unsupported_source")
	assert.Zero(t, g.count())
}
func TestBlockRefusedCodes(t *testing.T) {
	for _, code := range []int{3801, 3802, 368} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			_, _, id, a, g := actionFixture(t)
			g.status, g.response = 400, fmt.Sprintf(`{"error":{"code":%d}}`, code)
			got, err := actionRun(t, a, id, ActionBlock)
			actionCode(t, err, "block_refused")
			require.NotNil(t, got.Receipt)
			assert.Equal(t, "failed", got.Receipt.State)
		})
	}
}
func TestBlockResponseWithoutIDUnknown(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.response = `{"other":true}`
	got, err := actionRun(t, a, id, ActionBlock)
	actionCode(t, err, "outcome_unknown")
	require.NotNil(t, got.Receipt)
	assert.Equal(t, "unknown", got.Receipt.State)
	_, err = actionRun(t, a, id, ActionBlock)
	actionCode(t, err, "outcome_unknown")
	assert.Equal(t, 1, g.count())
}
func TestUnblockNoConfirm(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnblock})
	require.NoError(t, err)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnblock})
	require.NoError(t, err)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "300", rows[0].TargetAuthorID)
	assert.Equal(t, 2, g.count())
}
func TestBlockBindingIncludesRawAuthor(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock, DryRun: true})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRawWithFormat(id, []byte(`{"id":"100_200_300","from":{"id":"301","name":"Example Author"}}`), "facebook-comment-json"))
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock, Confirm: preview.Confirm})
	actionCode(t, err, "confirmation_mismatch")
	assert.Zero(t, g.count())
}

func TestBlockUnknownResolutionNoResend(t *testing.T) {
	for _, kind := range []ActionKind{ActionBlock, ActionUnblock} {
		for _, resolution := range []string{"sent", "not-sent"} {
			t.Run(string(kind)+"/"+resolution, func(t *testing.T) {
				_, _, id, a, g := actionFixture(t)
				g.status = 500
				got, err := actionRun(t, a, id, kind)
				actionCode(t, err, "outcome_unknown")
				_, err = actionRun(t, a, id, kind)
				actionCode(t, err, "outcome_unknown")
				resolved, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Confirm: got.Confirm, ResolveUnknown: resolution})
				require.NoError(t, err)
				state := "succeeded"
				if resolution == "not-sent" {
					state = "failed"
				}
				assert.Equal(t, state, resolved.Receipt.State)
				assert.Equal(t, 1, g.count())
				if resolution == "not-sent" {
					g.status, g.response = 200, `{"300":true}`
					if kind == ActionUnblock {
						g.response = `{"success":true}`
					}
					_, err = actionRun(t, a, id, kind)
					require.NoError(t, err)
					assert.Equal(t, 2, g.count())
				}
			})
		}
	}
}

func TestBlockAfterUnblockCanBlockAgain(t *testing.T) {
	for _, anotherComment := range []bool{false, true} {
		t.Run(fmt.Sprint(anotherComment), func(t *testing.T) {
			st, src, id, a, g := actionFixture(t)
			g.response = `{"300":true}`
			first, err := actionRun(t, a, id, ActionBlock)
			require.NoError(t, err)
			unblockID := id
			if anotherComment {
				other := message(t, st, src.ID, "100_200_300")
				other.ID, other.SourceMessageID = 0, "100_200_301"
				metadata, err := st.GetMessageMetadataContext(t.Context(), id)
				require.NoError(t, err)
				unblockID, err = st.UpsertMessage(other)
				require.NoError(t, err)
				require.NoError(t, st.SetMessageMetadataContext(t.Context(), unblockID, metadata))
				require.NoError(t, st.UpsertMessageRawWithFormat(unblockID, []byte(`{"id":"100_200_301","from":{"id":"300","name":"Example Author"}}`), "facebook-comment-json"))
			}
			g.response = `{"success":true}`
			_, err = a.Run(t.Context(), ActionRequest{MessageID: unblockID, Action: ActionUnblock})
			require.NoError(t, err)
			g.response = `{"300":true}`
			again, err := actionRun(t, a, id, ActionBlock)
			require.NoError(t, err)
			require.NotNil(t, again.Receipt)
			assert.NotEqual(t, first.Receipt.ID, again.Receipt.ID)
			assert.Equal(t, 3, g.count())
			_, err = actionRun(t, a, id, ActionBlock)
			require.NoError(t, err)
			assert.Equal(t, 3, g.count(), "same block generation remains idempotent")
		})
	}
}
func TestBlockUnknownAfterLaterUnblockStillResolves(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	unknown, err := actionRun(t, a, id, ActionBlock)
	actionCode(t, err, "outcome_unknown")
	g.status, g.response = 200, `{"success":true}`
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnblock})
	require.NoError(t, err)
	_, err = actionRun(t, a, id, ActionBlock)
	actionCode(t, err, "outcome_unknown")
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionBlock, Confirm: unknown.Confirm, ResolveUnknown: "not-sent"})
	require.NoError(t, err)
	assert.Equal(t, 2, g.count())
	g.response = `{"300":true}`
	_, err = actionRun(t, a, id, ActionBlock)
	require.NoError(t, err)
	assert.Equal(t, 3, g.count())
}

func TestBlockOlderUnblockResolutionDoesNotResend(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	uncertain, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnblock})
	actionCode(t, err, "outcome_unknown")
	g.status, g.response = 200, `{"300":true}`
	blocked, err := actionRun(t, a, id, ActionBlock)
	require.NoError(t, err)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionUnblock, Confirm: uncertain.Confirm, ResolveUnknown: "sent"})
	require.NoError(t, err)
	again, err := actionRun(t, a, id, ActionBlock)
	require.NoError(t, err)
	assert.Equal(t, blocked.Receipt.ID, again.Receipt.ID)
	assert.Equal(t, 2, g.count(), "resolving an older unblock is not a new provider change")
}
