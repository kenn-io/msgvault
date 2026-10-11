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
	st, before, folder := microsoftObservationFixture(t)
	revision := microsoftSourceRevision(t, st, before.Target.SourceID)
	after := before
	after.Tags = []string{}
	after.Revision = "version-2"
	after.ObservedAt = before.ObservedAt.Add(time.Second)
	changed, err := st.ObserveMicrosoftMailStateContext(t.Context(), after, &folder, &after.Tags)
	require.NoError(t, err)
	assert.False(t, changed, "result means folder changed")
	assert.Equal(t, []string{"Inbox"}, messageLabels(t, st, before.Target.ItemID))
	observed, err := st.GetInboxProviderState(t.Context(), before.Target)
	require.NoError(t, err)
	require.NotNil(t, observed)
	require.NotNil(t, observed.Read)
	assert.False(t, *observed.Read)
	assert.Empty(t, observed.Tags)
	assert.Equal(t, "version-2", observed.Revision, "complete fields distinguish known-empty from unknown after JSON")
	assert.Equal(t, revision+1, microsoftSourceRevision(t, st, before.Target.SourceID))
	unknown := inboxcontrol.State{Target: before.Target, ObservedAt: after.ObservedAt.Add(time.Second)}
	categories := []string{"Next"}
	_, err = st.ObserveMicrosoftMailStateContext(t.Context(), unknown, &folder, &categories)
	require.NoError(t, err)
	observed, err = st.GetInboxProviderState(t.Context(), before.Target)
	require.NoError(t, err)
	require.NotNil(t, observed)
	assert.Nil(t, observed.Read)
	assert.Nil(t, observed.Inbox)
	assert.Empty(t, observed.Revision)
	assert.ElementsMatch(t, []string{"Inbox", "Category: Next"}, messageLabels(t, st, before.Target.ItemID))
	assert.Equal(t, revision+2, microsoftSourceRevision(t, st, before.Target.SourceID))
	var localRead bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), before.Target.ItemID).Scan(&localRead))
	assert.True(t, localRead, "native read=false must preserve the local UI read=true")
}

func TestMicrosoftInboxObservationRequiresExactFolderMapping(t *testing.T) {
	for _, kind := range []string{"missing", "wrong", "foreign", "category"} {
		t.Run(kind, func(t *testing.T) {
			st, before, _ := microsoftObservationFixture(t)
			revision := microsoftSourceRevision(t, st, before.Target.SourceID)
			var mapping *int64
			switch kind {
			case "wrong":
				id, err := st.LabelIDContext(t.Context(), before.Target.SourceID, "archive-id")
				require.NoError(t, err)
				mapping = &id
			case "foreign":
				source, err := st.GetOrCreateSource("msmail", "other@example.test")
				require.NoError(t, err)
				folders, err := st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{"inbox-id": {Name: "Inbox", Type: "system"}})
				require.NoError(t, err)
				id := folders["inbox-id"]
				mapping = &id
			case "category":
				id, err := st.LabelIDContext(t.Context(), before.Target.SourceID, "msmail-category:Old")
				require.NoError(t, err)
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
			require.Error(t, err)
			observed, err := st.GetInboxProviderState(t.Context(), before.Target)
			require.NoError(t, err)
			assert.Equal(t, before, *observed)
			assert.ElementsMatch(t, []string{"Inbox", "Category: Old"}, messageLabels(t, st, before.Target.ItemID))
			assert.Equal(t, revision, microsoftSourceRevision(t, st, before.Target.SourceID))
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
			assert.ErrorIs(t, err, inboxcontrol.ErrConflict)
			assert.ElementsMatch(t, []string{"Inbox", "Category: Old"}, messageLabels(t, st, before.Target.ItemID))
			assert.Equal(t, revision, microsoftSourceRevision(t, st, before.Target.SourceID))
		})
	}
}

func TestMicrosoftInboxObservationRollback(t *testing.T) {
	for _, table := range []string{"inbox_provider_states", "inbox_source_revisions"} {
		t.Run(table, func(t *testing.T) {
			st, before, folder := microsoftObservationFixture(t)
			revision := microsoftSourceRevision(t, st, before.Target.SourceID)
			var err error
			if st.IsPostgreSQL() {
				_, err = st.DB().Exec(`CREATE FUNCTION microsoft_observation_failure() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'synthetic observation failure'; END; $$ LANGUAGE plpgsql`)
				require.NoError(t, err)
				_, err = st.DB().Exec(`CREATE TRIGGER microsoft_observation_failure BEFORE INSERT ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION microsoft_observation_failure()`)
			} else {
				_, err = st.DB().Exec(`CREATE TRIGGER microsoft_observation_failure BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'synthetic observation failure'); END`)
			}
			require.NoError(t, err)
			after := before
			after.Tags = []string{"Next"}
			after.Revision = "version-2"
			after.ObservedAt = before.ObservedAt.Add(time.Second)
			_, err = st.ObserveMicrosoftMailStateContext(t.Context(), after, &folder, &after.Tags)
			require.ErrorContains(t, err, "synthetic observation failure")
			observed, err := st.GetInboxProviderState(t.Context(), before.Target)
			require.NoError(t, err)
			assert.Equal(t, before, *observed)
			assert.ElementsMatch(t, []string{"Inbox", "Category: Old"}, messageLabels(t, st, before.Target.ItemID))
			assert.Equal(t, revision, microsoftSourceRevision(t, st, before.Target.SourceID))
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
