package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestXAuthorProtectionSurvivesSourceRemoval(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "serialized"}[serialized], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t).Store
			source, err := st.GetOrCreateSource("x", "9000")
			require.NoError(err)
			at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: "1001", Protected: true, FetchedAt: at})
			require.NoError(err)
			if serialized {
				_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
			} else {
				err = st.RemoveSource(source.ID)
			}
			require.NoError(err)
			other, err := st.GetOrCreateSource("x", "9001")
			require.NoError(err)
			pid, err := st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: other.ID, UserID: "1001", Username: "old_handle", DisplayName: "Old Public Label", FetchedAt: at.Add(-time.Minute)})
			require.NoError(err)
			var name sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
			assert.Empty(name.String)
			var count int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username'`), pid).Scan(&count))
			assert.Zero(count)
			_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: other.ID, UserID: "1001", Username: "new_handle", DisplayName: "New Public Label", FetchedAt: at.Add(time.Minute)})
			require.NoError(err)
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
			assert.Equal("New Public Label", name.String)
		})
	}
}

func TestXProfileRemovalBlocksDeletingForeignProfileSource(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "serialized"}[serialized], func(t *testing.T) {
			require := require.New(t)
			st := storetest.New(t).Store
			observer, err := st.GetOrCreateSource("x", "9000")
			require.NoError(err)
			profile, err := st.GetOrCreateSource("x", "9001")
			require.NoError(err)
			at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			_, err = st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: profile.ID, UserID: "1001", Username: "former_handle", DisplayName: "Former Public Label", FetchedAt: at})
			require.NoError(err)
			require.NoError(st.QueueXAuthorRemovalContext(t.Context(), observer.ID, "1001", at.Add(time.Minute)))
			if serialized {
				_, _, err = st.RemoveSourceSerialized(t.Context(), profile.ID)
			} else {
				err = st.RemoveSource(profile.ID)
			}
			require.ErrorIs(err, store.ErrXRemovalPending, "deleting foreign profile evidence must not strand its label before cleanup")
		})
	}
}
