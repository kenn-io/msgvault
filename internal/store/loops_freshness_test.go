package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopFreshnessAdapters(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	types := store.AuthenticatedChatSourceTypes()
	requirements.Contains(types, "whatsapp")
	types[0] = "changed"
	assertions.NotContains(store.AuthenticatedChatSourceTypes(), "changed")
	outcome, err := f.Store.LatestSyncOutcomeContext(t.Context(), f.Source.ID)
	requirements.NoError(err)
	assertions.Equal("never", outcome)
	id := f.StartSync()
	outcome, err = f.Store.LatestSyncOutcomeContext(t.Context(), f.Source.ID)
	requirements.NoError(err)
	assertions.Equal("running", outcome)
	loopExec(t, f.Store, "UPDATE sync_runs SET status='completed', errors_count=1 WHERE id=?", id)
	outcome, err = f.Store.LatestSyncOutcomeContext(t.Context(), f.Source.ID)
	requirements.NoError(err)
	assertions.Equal("partial", outcome)
	at, err := f.Store.ActivityProjectionComputedAtContext(t.Context(), nil)
	requirements.NoError(err)
	assertions.Nil(at)
	var person int64
	requirements.NoError(f.Store.DB().QueryRow("INSERT INTO persons(vcard_uid) VALUES('freshness-person') RETURNING id").Scan(&person))
	at, err = f.Store.ActivityProjectionComputedAtContext(t.Context(), []int64{person})
	requirements.NoError(err)
	assertions.Nil(at)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	loopExec(t, f.Store, "INSERT INTO person_contact_state(person_id,computed_at) VALUES(?,?)", person, now)
	at, err = f.Store.ActivityProjectionComputedAtContext(t.Context(), []int64{person})
	requirements.NoError(err)
	requirements.NotNil(at)
	assertions.True(now.Equal(*at))

	var second, unrelated int64
	requirements.NoError(f.Store.DB().QueryRow("INSERT INTO persons(vcard_uid) VALUES('freshness-second') RETURNING id").Scan(&second))
	requirements.NoError(f.Store.DB().QueryRow("INSERT INTO persons(vcard_uid) VALUES('freshness-unrelated') RETURNING id").Scan(&unrelated))
	later := now.Add(time.Hour)
	loopExec(t, f.Store, "INSERT INTO person_contact_state(person_id,computed_at) VALUES(?,?)", second, later)
	loopExec(t, f.Store, "INSERT INTO person_contact_state(person_id,computed_at) VALUES(?,?)", unrelated, later.Add(time.Hour))
	at, err = f.Store.ActivityProjectionComputedAtContext(t.Context(), []int64{second, person})
	requirements.NoError(err)
	requirements.NotNil(at)
	assertions.True(later.Equal(*at), "use the newest computation only within the requested people")
}
