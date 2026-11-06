package keepintouch

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"go.kenn.io/msgvault/internal/taskclient"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type outreachEvidenceArchive struct {
	*store.Store

	options store.PersonTierOptions
}

func (a outreachEvidenceArchive) ContactStateContext(ctx context.Context, id int64, now time.Time) (store.ContactState, error) {
	return a.KeepInTouchContactStateContext(ctx, id, now, a.options)
}

type outreachEvidenceRunner func(context.Context, peoplesweep.StructuredRequest) (peoplesweep.StructuredResponse, error)

func (r outreachEvidenceRunner) RunStructured(ctx context.Context, req peoplesweep.StructuredRequest) (peoplesweep.StructuredResponse, error) {
	return r(ctx, req)
}

// Only the
// provider boundary is scripted; Store, projector, brief builder and Kata are real.
func TestOutreachChangedEvidenceDefersPublication(t *testing.T) {
	for _, change := range []string{"deleted", "identity", "pending_delivery"} {
		t.Run(change, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)

			s := outreachFixture(t).Service
			f := storetest.New(t)
			st := f.Store
			source, err := st.GetOrCreateSource("slack", "synthetic-review")
			require.NoError(err)
			conv, err := st.EnsureConversationWithType(source.ID, "synthetic-review-chat", "direct_chat", "Synthetic chat")
			require.NoError(err)
			owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
			other := f.EnsureParticipant("person@example.test", "Example Person", "example.test")
			require.NoError(st.AddAccountIdentity(source.ID, "owner@example.test", "manual"))
			person, _, err := st.CreatePersonFromParticipant(other)
			require.NoError(err)
			for _, id := range []int64{owner, other} {
				require.NoError(st.EnsureConversationParticipant(conv, id, "member"))
			}
			last := s.Clock().AddDate(0, 0, -30)
			insert := func(key, body string, at time.Time) int64 {
				id, e := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: key, ConversationID: conv, MessageType: "slack", SenderID: sql.NullInt64{Int64: other, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}})
				Require.NoError(t, e)
				Require.NoError(t, st.ReplaceMessageRecipients(id, "to", []int64{owner}, []string{"Owner"}))
				Require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
				return id
			}
			older := insert("older-evidence", "I started learning to paint.", last.AddDate(0, 0, -1))
			insert("latest-contact", "Hello again.", last)
			days := int64(21)
			_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
			require.NoError(err)
			projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
			require.NoError(err)
			_, err = projector.RunOnce(t.Context())
			require.NoError(err)
			archive := outreachEvidenceArchive{st, store.PersonTierOptions{Config: persontiers.DefaultConfig(), ActivityTimezone: "UTC", ActivityMaxDirect: 25}}
			before, err := archive.ContactStateContext(t.Context(), person.ID, s.Clock())
			require.NoError(err)
			require.True(Eligible(before, s.Clock(), 7), "fixture must enter real production path: %+v", before)
			runner := outreachEvidenceRunner(func(ctx context.Context, req peoplesweep.StructuredRequest) (peoplesweep.StructuredResponse, error) {
				Require.Contains(t, req.InputText, "I started learning to paint.")
				switch change {
				case "identity":
					Require.NoError(t, st.AddAccountIdentity(source.ID, "another-owner@example.test", "manual"))
				case "deleted":
					Require.NoError(t, st.MarkMessageDeleted(source.ID, "older-evidence"))
				}
				_, e := projector.RunOnce(ctx)
				Require.NoError(t, e)
				after, e := archive.ContactStateContext(ctx, person.ID, s.Clock())
				Require.NoError(t, e)
				Require.False(t, after.Stale)
				Require.Equal(t, before.LastContactAt, after.LastContactAt)
				Require.Equal(t, before.CadenceDueAt, after.CadenceDueAt)
				_, e = st.HydratePersonSweepMessages(ctx, person.ID, []int64{older}, 0)
				if change == "deleted" {
					Require.ErrorIs(t, e, peoplesweep.ErrPersonSweepMessageUnavailable)
				} else {
					Require.NoError(t, e)
				}
				return peoplesweep.StructuredResponse{Output: []byte(fmt.Sprintf(`{"text":"How is the painting going?","message_ids":[%d]}`, older))}, nil
			})
			s.Archive = archive
			s.Drafts = &ProductionDrafts{Archive: archive, Profile: peoplesweep.ProviderProfile{Fingerprint: "synthetic-profile", AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2026-01-01", AllowSensitive: true}, NewRunner: func() (TextRunner, error) { return runner, nil }}
			originalKata := s.Kata
			if change == "pending_delivery" {
				s.Kata = failedOutreachDelivery{Kata: originalKata}
			}
			result, err := s.Check(t.Context(), false)
			if change == "pending_delivery" {
				require.Error(err)
				tasks, e := originalKata.ListPersonTasksComplete(t.Context(), person.VCardUID)
				require.NoError(e)
				require.Len(tasks, 1)
				require.NoError(st.MarkMessageDeleted(source.ID, "older-evidence"))
				_, e = projector.RunOnce(t.Context())
				require.NoError(e)
				s.Kata = originalKata
				result, err = s.Check(t.Context(), false)
				remaining, e := originalKata.ListPersonTasksComplete(t.Context(), person.VCardUID)
				require.NoError(e)
				require.Len(remaining, 1)
				assert.Equal(tasks[0].Body, remaining[0].Body)
			}
			require.NoError(err, "%+v", result.Failures)
			assert.Empty(result.Items, "changed evidence must not authorize publication or delivery")
		})
	}
}

type failedOutreachDelivery struct{ Kata }

func (failedOutreachDelivery) NotifyTaskOnce(context.Context, taskclient.KataTask, string, string) (taskclient.KataTask, error) {
	return taskclient.KataTask{}, errors.New("synthetic delivery outage")
}

// Native inbox uses the all-issue metadata filter; verify this consumer path,
// rather than merely looking for the key in a single GetTask response.
func TestOutreachNativeInboxConsumerFindsDelivery(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	fixture := outreachFixture(t)

	s, server := fixture.Service, fixture.Server
	result, err := s.Check(t.Context(), false)
	require.NoError(err)
	require.Len(result.Items, 1)
	query := url.Values{"meta": {"notify." + base64.RawURLEncoding.EncodeToString([]byte("operator"))}, "status": {"open"}, "limit": {"0"}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/issues?"+query.Encode(), nil)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer "+katatest.Token)
	resp, err := server.Client().Do(req)
	require.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode)
	assert.Contains(string(body), result.Items[0].Issue.UID)
	assert.Contains(string(body), "Review the keep-in-touch draft before sending.")
}
