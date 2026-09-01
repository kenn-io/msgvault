package testutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/msgvault/internal/store"
)

// A SQLite fixture used to open an empty file and replay InitSchema() into it:
// a few hundred DDL statements, ~140ms on a laptop and most of a second on a
// contended CI runner, paid by every one of the ~1000 fixtures in the suite.
// The initialized schema is a pure function of the test binary, so it is built
// once per binary and every later fixture receives a byte copy of that file.
// Each fixture still gets a private database produced by the same InitSchema()
// path; only the replay is shared.
var sqliteTemplate struct {
	once sync.Once
	data []byte
	err  error
}

// sqliteTemplateBytes returns the bytes of a freshly initialized SQLite
// database, building it on first use.
func sqliteTemplateBytes() ([]byte, error) {
	sqliteTemplate.once.Do(func() {
		sqliteTemplate.data, sqliteTemplate.err = buildSQLiteTemplate()
	})

	return sqliteTemplate.data, sqliteTemplate.err
}

// buildSQLiteTemplate initializes a database in a scratch directory and returns
// its bytes. The directory is removed before returning, so nothing outlives the
// process. Close folds the WAL back into the main file; a WAL still holding
// data afterwards would make the snapshot incomplete, so that is a build
// failure rather than a fixture handed out with missing tables.
func buildSQLiteTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "msgvault-sqlite-template-")
	if err != nil {
		return nil, fmt.Errorf("template scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "template.db")
	st, err := store.OpenForTest(path)
	if err != nil {
		return nil, fmt.Errorf("open template: %w", err)
	}
	if err := st.InitSchema(); err != nil {
		_ = st.Close()

		return nil, fmt.Errorf("init template schema: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("close template: %w", err)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		return nil, errors.New("template WAL was not folded into the main file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}

	return data, nil
}
