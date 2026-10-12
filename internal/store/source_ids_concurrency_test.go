package store_test

import (
	"encoding/json/v2"
	"fmt"
	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestSourceLifetimeRollbackAndNoShrink(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite retained metadata; PG identity is tested separately")
	}
	first, err := st.GetOrCreateSource("gmail", "first@example.test")
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value='1000' WHERE key='source_id_high_water'`)
	require.NoError(err)
	got, err := st.GetOrCreateSource("gmail", first.Identifier)
	require.NoError(err)
	assert.Equal(first.ID, got.ID)
	_, err = st.DB().Exec(`CREATE TRIGGER reject_new_source BEFORE INSERT ON sources BEGIN SELECT RAISE(ABORT,'synthetic insert rollback'); END`)
	require.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "rejected@example.test")
	require.ErrorContains(err, "synthetic insert rollback")
	var counter string
	require.NoError(st.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='source_id_high_water'`).Scan(&counter))
	assert.Equal("1000", counter)
	_, err = st.DB().Exec(`DROP TRIGGER reject_new_source`)
	require.NoError(err)
	next, err := st.GetOrCreateSource("gmail", "next@example.test")
	require.NoError(err)
	assert.Equal(int64(1001), next.ID)
	for _, serialized := range []bool{false, true} {
		_, err = st.DB().Exec(`DELETE FROM archive_metadata WHERE key='source_id_high_water'`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER reject_source_delete BEFORE DELETE ON sources BEGIN SELECT RAISE(ABORT,'synthetic delete rollback'); END`)
		require.NoError(err)
		if serialized {
			_, _, err = st.RemoveSourceSerialized(t.Context(), next.ID)
		} else {
			err = st.RemoveSource(next.ID)
		}
		require.ErrorContains(err, "synthetic delete rollback")
		var count int
		require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key='source_id_high_water'`).Scan(&count))
		assert.Zero(count, "failed removal must roll back first counter initialization")
		_, err = st.DB().Exec(`DROP TRIGGER reject_source_delete`)
		require.NoError(err)
	}
}

func TestSourceLifetimeIndependentStoresConvergeAndReopen(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	initial, err := store.Open(path)
	require.NoError(err)
	require.NoError(initial.InitSchema())
	require.NoError(initial.Close())
	stores := make([]*store.Store, 4)
	for i := range stores {
		stores[i], err = store.Open(path)
		require.NoError(err)
		defer func() { require.NoError(stores[i].Close()) }()
	}
	type result struct {
		source *store.Source
		err    error
	}
	for _, same := range []bool{true, false} {
		start := make(chan struct{})
		results := make(chan result, len(stores))
		var wg sync.WaitGroup
		for i, st := range stores {
			wg.Go(func() {
				<-start
				identifier := "shared@example.test"
				if !same {
					identifier = fmt.Sprintf("worker%d@example.test", i)
				}
				s, e := st.GetOrCreateSourceContext(t.Context(), "gmail", identifier)
				results <- result{s, e}
			})
		}
		close(start)
		wg.Wait()
		close(results)
		ids := map[int64]bool{}
		for result := range results {
			require.NoError(result.err)
			require.NotNil(result.source)
			ids[result.source.ID] = true
		}
		if same {
			assert.Len(ids, 1)
		} else {
			assert.Len(ids, len(stores))
		}
	}
	highest, err := stores[0].GetOrCreateSource("gmail", "last@example.test")
	require.NoError(err)
	require.NoError(stores[0].RemoveSource(highest.ID))
	reopened, err := store.Open(path)
	require.NoError(err)
	defer func() { require.NoError(reopened.Close()) }()
	next, err := reopened.GetOrCreateSource("gmail", highest.Identifier)
	require.NoError(err)
	assert.Greater(next.ID, highest.ID)
}

func TestSourceLifetimeIndependentProcess(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.Open(path)
	require.NoError(err)
	require.NoError(st.InitSchema())
	first, err := st.GetOrCreateSource("gmail", "before@example.test")
	require.NoError(err)
	require.NoError(st.RemoveSource(first.ID))
	require.NoError(st.Close())
	executable, err := os.Executable()
	require.NoError(err)
	for _, same := range []bool{false, true} {
		commands := make([]*exec.Cmd, 2)
		outputs := make([]string, 2)
		for i := range commands {
			outputs[i] = filepath.Join(t.TempDir(), "result.json")
			identifier := fmt.Sprintf("process%d@example.test", i)
			if same {
				identifier = "same-process@example.test"
			}
			commands[i] = exec.CommandContext(t.Context(), executable, "-test.run=^TestSourceLifetimeProcessChild$")
			commands[i].Env = append(os.Environ(), "GOMAXPROCS=1", "MSGVAULT_SOURCE_LIFETIME_PATH="+path, "MSGVAULT_SOURCE_LIFETIME_IDENTIFIER="+identifier, "MSGVAULT_SOURCE_LIFETIME_OUTPUT="+outputs[i])
			require.NoError(commands[i].Start())
		}
		ids := make([]int64, 2)
		for i, c := range commands {
			require.NoError(c.Wait())
			data, err := os.ReadFile(outputs[i])
			require.NoError(err)
			require.NoError(json.Unmarshal(data, &ids[i]))
			assert.Greater(ids[i], first.ID)
		}
		if same {
			assert.Equal(ids[0], ids[1])
		} else {
			assert.NotEqual(ids[0], ids[1])
		}
	}
}

func TestSourceLifetimeProcessChild(t *testing.T) {
	path := os.Getenv("MSGVAULT_SOURCE_LIFETIME_PATH")
	if path == "" {
		return
	}
	require := Require.New(t)
	st, err := store.Open(path)
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	source, err := st.GetOrCreateSourceContext(t.Context(), "gmail", os.Getenv("MSGVAULT_SOURCE_LIFETIME_IDENTIFIER"))
	require.NoError(err)
	data, err := json.Marshal(source.ID)
	require.NoError(err)
	require.NoError(os.WriteFile(os.Getenv("MSGVAULT_SOURCE_LIFETIME_OUTPUT"), data, 0600))
}
