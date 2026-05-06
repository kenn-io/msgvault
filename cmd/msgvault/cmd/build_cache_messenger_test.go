package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/wesm/msgvault/internal/fbmessenger"
	"github.com/wesm/msgvault/internal/store"
)

// TestBuildCache_AfterMessengerImport verifies invariant #3 from the plan:
// after importing Messenger JSON and running buildCache, the resulting
// Parquet partition files exist and contain the expected row count.
func TestBuildCache_AfterMessengerImport(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "msgvault.db")
	analyticsDir := filepath.Join(tmp, "analytics")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	fixture, err := filepath.Abs("../../../internal/fbmessenger/testdata/json_simple")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := fbmessenger.ImportDYI(context.Background(), st, fbmessenger.ImportOptions{
		Me:             "wes@facebook.messenger",
		RootDir:        fixture,
		Format:         "auto",
		AttachmentsDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ImportDYI: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if summary.MessagesAdded != 4 {
		t.Fatalf("imported %d want 4", summary.MessagesAdded)
	}

	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("buildCache: %v", err)
	}
	if result.Skipped {
		t.Fatal("buildCache unexpectedly skipped")
	}

	duckdb, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckdb.Close() }()

	var n int
	pattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	if err := duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?, hive_partitioning=true)`, pattern,
	).Scan(&n); err != nil {
		t.Fatalf("duckdb scan: %v", err)
	}
	if n != 4 {
		t.Errorf("parquet messages=%d want 4", n)
	}

	var mtype string
	if err := duckdb.QueryRow(
		`SELECT DISTINCT message_type FROM read_parquet(?, hive_partitioning=true)`, pattern,
	).Scan(&mtype); err != nil {
		t.Fatalf("duckdb message_type: %v", err)
	}
	if mtype != "fbmessenger" {
		t.Errorf("message_type=%q want fbmessenger", mtype)
	}
}
