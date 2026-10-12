package store_test

import (
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestSourceLifetimeRollbackAndNoShrink(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite retained metadata; PG identity is tested separately")
	}
	first, err := st.GetOrCreateSource("gmail", "first@example.test")
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE archive_metadata SET value='1000' WHERE key='source_id_high_water'`)
	requirements.NoError(err)
	got, err := st.GetOrCreateSource("gmail", first.Identifier)
	requirements.NoError(err)
	assertions.Equal(first.ID, got.ID)
	_, err = st.DB().Exec(`CREATE TRIGGER reject_new_source BEFORE INSERT ON sources BEGIN SELECT RAISE(ABORT,'synthetic insert rollback'); END`)
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "rejected@example.test")
	requirements.ErrorContains(err, "synthetic insert rollback")
	var counter string
	requirements.NoError(st.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='source_id_high_water'`).Scan(&counter))
	assertions.Equal("1000", counter)
	_, err = st.DB().Exec(`DROP TRIGGER reject_new_source`)
	requirements.NoError(err)
	next, err := st.GetOrCreateSource("gmail", "next@example.test")
	requirements.NoError(err)
	assertions.Equal(int64(1001), next.ID)
	for _, serialized := range []bool{false, true} {
		_, err = st.DB().Exec(`DELETE FROM archive_metadata WHERE key='source_id_high_water'`)
		requirements.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER reject_source_delete BEFORE DELETE ON sources BEGIN SELECT RAISE(ABORT,'synthetic delete rollback'); END`)
		requirements.NoError(err)
		if serialized {
			_, _, err = st.RemoveSourceSerialized(t.Context(), next.ID)
		} else {
			err = st.RemoveSource(next.ID)
		}
		requirements.ErrorContains(err, "synthetic delete rollback")
		var count int
		requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key='source_id_high_water'`).Scan(&count))
		assertions.Zero(count, "failed removal must roll back first counter initialization")
		_, err = st.DB().Exec(`DROP TRIGGER reject_source_delete`)
		requirements.NoError(err)
	}
}

func TestSourceLifetimeIndependentStoresConvergeAndReopen(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	initial, err := store.Open(path)
	requirements.NoError(err)
	requirements.NoError(initial.InitSchema())
	requirements.NoError(initial.Close())
	stores := make([]*store.Store, 4)
	for i := range stores {
		stores[i], err = store.Open(path)
		requirements.NoError(err)
		defer func() { requirements.NoError(stores[i].Close()) }()
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
			requirements.NoError(result.err)
			requirements.NotNil(result.source)
			ids[result.source.ID] = true
		}
		if same {
			assertions.Len(ids, 1)
		} else {
			assertions.Len(ids, len(stores))
		}
	}
	highest, err := stores[0].GetOrCreateSource("gmail", "last@example.test")
	requirements.NoError(err)
	requirements.NoError(stores[0].RemoveSource(highest.ID))
	reopened, err := store.Open(path)
	requirements.NoError(err)
	defer func() { requirements.NoError(reopened.Close()) }()
	next, err := reopened.GetOrCreateSource("gmail", highest.Identifier)
	requirements.NoError(err)
	assertions.Greater(next.ID, highest.ID)
}

func TestSourceLifetimeIndependentProcess(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.Open(path)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	first, err := st.GetOrCreateSource("gmail", "before@example.test")
	requirements.NoError(err)
	requirements.NoError(st.RemoveSource(first.ID))
	requirements.NoError(st.Close())
	executable, err := os.Executable()
	requirements.NoError(err)
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
			requirements.NoError(commands[i].Start())
		}
		ids := make([]int64, 2)
		for i, c := range commands {
			requirements.NoError(c.Wait())
			data, err := os.ReadFile(outputs[i])
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(data, &ids[i]))
			assertions.Greater(ids[i], first.ID)
		}
		if same {
			assertions.Equal(ids[0], ids[1])
		} else {
			assertions.NotEqual(ids[0], ids[1])
		}
	}
}

func TestSourceLifetimeProcessChild(t *testing.T) {
	path := os.Getenv("MSGVAULT_SOURCE_LIFETIME_PATH")
	if path == "" {
		return
	}
	requirements := require.New(t)
	st, err := store.Open(path)
	requirements.NoError(err)
	defer func() { requirements.NoError(st.Close()) }()
	source, err := st.GetOrCreateSourceContext(t.Context(), "gmail", os.Getenv("MSGVAULT_SOURCE_LIFETIME_IDENTIFIER"))
	requirements.NoError(err)
	data, err := json.Marshal(source.ID)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(os.Getenv("MSGVAULT_SOURCE_LIFETIME_OUTPUT"), data, 0600))
}
