package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

func microsoftObservationFixture(t *testing.T) (*store.Store, inboxcontrol.State, int64) {
	t.Helper()
	st, source, id := tagFixture(t, "msmail", "immutable-1")
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=TRUE WHERE id=?`), id)
	require.NoError(t, err)
	folders, err := st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{"inbox-id": {Name: "Inbox", Type: "system"}, "archive-id": {Name: "Archive", Type: "user"}})
	require.NoError(t, err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "msmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "immutable-1"}
	state := inboxcontrol.State{Target: target, Tags: []string{"Old"}, Read: new(false), Inbox: new(true), Location: "inbox-id", Revision: "version-1", ObservedAt: time.Now().UTC()}
	folder := folders["inbox-id"]
	_, err = st.ObserveMicrosoftMailStateContext(t.Context(), state, &folder, &state.Tags)
	require.NoError(t, err)
	return st, state, folder
}

func microsoftSourceRevision(t *testing.T, st *store.Store, sourceID int64) int64 {
	t.Helper()
	var revision int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT revision FROM inbox_source_revisions WHERE source_id=?`), sourceID).Scan(&revision))
	return revision
}

func TestMicrosoftInboxObservationKnownEmptyAndPartial(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, before, folder := microsoftObservationFixture(t)
	revision := microsoftSourceRevision(t, st, before.Target.SourceID)
	after := before
	after.Tags = []string{}
	after.Revision = "version-2"
	after.ObservedAt = before.ObservedAt.Add(time.Second)
	changed, err := st.ObserveMicrosoftMailStateContext(t.Context(), after, &folder, &after.Tags)
	requirements.NoError(err)
	assertions.False(changed, "result means folder changed")
	assertions.Equal([]string{"Inbox"}, messageLabels(t, st, before.Target.ItemID))
	observed, err := st.GetInboxProviderState(t.Context(), before.Target)
	requirements.NoError(err)
	requirements.NotNil(observed)
	requirements.NotNil(observed.Read)
	assertions.False(*observed.Read)
	assertions.NotNil(observed.Tags)
	assertions.Equal([]string{}, observed.Tags)
	assertions.Equal("version-2", observed.Revision, "complete fields distinguish known-empty from unknown after JSON")
	assertions.Equal(revision+1, microsoftSourceRevision(t, st, before.Target.SourceID))
	unknown := inboxcontrol.State{Target: before.Target, ObservedAt: after.ObservedAt.Add(time.Second)}
	categories := []string{"Next"}
	_, err = st.ObserveMicrosoftMailStateContext(t.Context(), unknown, &folder, &categories)
	requirements.NoError(err)
	observed, err = st.GetInboxProviderState(t.Context(), before.Target)
	requirements.NoError(err)
	requirements.NotNil(observed)
	assertions.Nil(observed.Read)
	assertions.Nil(observed.Inbox)
	assertions.Empty(observed.Revision)
	assertions.ElementsMatch([]string{"Inbox", "Category: Next"}, messageLabels(t, st, before.Target.ItemID))
	assertions.Equal(revision+2, microsoftSourceRevision(t, st, before.Target.SourceID))
	var localRead bool
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), before.Target.ItemID).Scan(&localRead))
	assertions.True(localRead, "native read=false must preserve the local UI read=true")
}

func TestMicrosoftInboxObservationPreservesTombstones(t *testing.T) {
	for _, column := range []string{"deleted_at", "deleted_from_source_at"} {
		t.Run(column, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, before, folder := microsoftObservationFixture(t)
			_, err := st.DB().Exec(st.Rebind("UPDATE messages SET "+column+"=CURRENT_TIMESTAMP WHERE id=?"), before.Target.ItemID)
			requirements.NoError(err)
			requirements.ErrorIs(st.ValidateInboxTargetContext(t.Context(), before.Target), inboxcontrol.ErrInvalid, "provider operations still require a live item")

			after := before
			after.Tags = []string{"Next"}
			after.Revision = "version-2"
			after.ObservedAt = before.ObservedAt.Add(time.Second)
			changed, err := st.ObserveMicrosoftMailStateContext(t.Context(), after, &folder, &after.Tags)
			requirements.NoError(err, "sync may refresh metadata for a retained Microsoft message")
			assertions.False(changed, "result means folder changed; categories can reconcile independently")
			observed, err := st.GetInboxProviderState(t.Context(), before.Target)
			requirements.NoError(err)
			requirements.NotNil(observed)
			assertions.Equal(after, *observed)
			assertions.ElementsMatch([]string{"Inbox", "Category: Next"}, messageLabels(t, st, before.Target.ItemID))
			var stillTombstoned int
			requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE id=? AND "+column+" IS NOT NULL"), before.Target.ItemID).Scan(&stillTombstoned))
			assertions.Equal(1, stillTombstoned, "metadata refresh preserves the deletion marker")
		})
	}
}

func TestMicrosoftInboxObservationRequiresExactFolderMapping(t *testing.T) {
	for _, kind := range []string{"missing", "wrong", "foreign", "category"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, before, _ := microsoftObservationFixture(t)
			revision := microsoftSourceRevision(t, st, before.Target.SourceID)
			var mapping *int64
			switch kind {
			case "wrong":
				id, err := st.LabelIDContext(t.Context(), before.Target.SourceID, "archive-id")
				requirements.NoError(err)
				mapping = &id
			case "foreign":
				source, err := st.GetOrCreateSource("msmail", "other@example.test")
				requirements.NoError(err)
				folders, err := st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{"inbox-id": {Name: "Inbox", Type: "system"}})
				requirements.NoError(err)
				id := folders["inbox-id"]
				mapping = &id
			case "category":
				id, err := st.LabelIDContext(t.Context(), before.Target.SourceID, "msmail-category:Old")
				requirements.NoError(err)
				mapping = &id
			}
			after := before
			if kind == "category" {
				after.Location = "msmail-category:Old"
			}
			after.Tags = []string{"Next"}
			after.Revision = "version-2"
			after.ObservedAt = before.ObservedAt.Add(time.Second)
			_, err := st.ObserveMicrosoftMailStateContext(t.Context(), after, mapping, &after.Tags)
			requirements.Error(err)
			observed, err := st.GetInboxProviderState(t.Context(), before.Target)
			requirements.NoError(err)
			assertions.Equal(before, *observed)
			assertions.ElementsMatch([]string{"Inbox", "Category: Old"}, messageLabels(t, st, before.Target.ItemID))
			assertions.Equal(revision, microsoftSourceRevision(t, st, before.Target.SourceID))
		})
	}
}

func TestMicrosoftInboxObservationFreshnessBeforeProjection(t *testing.T) {
	for _, delta := range []time.Duration{-time.Second, 0} {
		t.Run(delta.String(), func(t *testing.T) {
			st, before, folder := microsoftObservationFixture(t)
			revision := microsoftSourceRevision(t, st, before.Target.SourceID)
			after := before
			after.Tags = []string{"Next"}
			after.ObservedAt = before.ObservedAt.Add(delta)
			_, err := st.ObserveMicrosoftMailStateContext(t.Context(), after, &folder, &after.Tags)
			require.ErrorIs(t, err, inboxcontrol.ErrConflict)
			assert.ElementsMatch(t, []string{"Inbox", "Category: Old"}, messageLabels(t, st, before.Target.ItemID))
			assert.Equal(t, revision, microsoftSourceRevision(t, st, before.Target.SourceID))
		})
	}
}

func TestMicrosoftInboxObservationRollback(t *testing.T) {
	for _, table := range []string{"inbox_provider_states", "inbox_source_revisions"} {
		t.Run(table, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st, before, folder := microsoftObservationFixture(t)
			revision := microsoftSourceRevision(t, st, before.Target.SourceID)
			var err error
			if st.IsPostgreSQL() {
				_, err = st.DB().Exec(`CREATE FUNCTION microsoft_observation_failure() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic observation failure'; END; $$ LANGUAGE plpgsql`)
				requirements.NoError(err)
				_, err = st.DB().Exec(`CREATE TRIGGER microsoft_observation_failure BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION microsoft_observation_failure()`)
			} else {
				_, err = st.DB().Exec(`CREATE TRIGGER microsoft_observation_failure BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'synthetic observation failure'); END`)
			}
			requirements.NoError(err)
			after := before
			after.Tags = []string{"Next"}
			after.Revision = "version-2"
			after.ObservedAt = before.ObservedAt.Add(time.Second)
			_, err = st.ObserveMicrosoftMailStateContext(t.Context(), after, &folder, &after.Tags)
			requirements.ErrorContains(err, "synthetic observation failure")
			observed, err := st.GetInboxProviderState(t.Context(), before.Target)
			requirements.NoError(err)
			assertions.Equal(before, *observed)
			assertions.ElementsMatch([]string{"Inbox", "Category: Old"}, messageLabels(t, st, before.Target.ItemID))
			assertions.Equal(revision, microsoftSourceRevision(t, st, before.Target.SourceID))
		})
	}
}

func TestMicrosoftInboxReconcileEmptyReceiptCategories(t *testing.T) {
	st, before, _ := microsoftObservationFixture(t)
	after := before
	after.Tags = nil
	after.Revision = "version-2"
	after.ObservedAt = before.ObservedAt.Add(time.Second)
	require.NoError(t, st.ReconcileInboxProviderState(t.Context(), before.Target, before, after))
	assert.Equal(t, []string{"Inbox"}, messageLabels(t, st, before.Target.ItemID))
}
