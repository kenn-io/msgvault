//go:build sqlite_vec

package hybrid

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/wesm/msgvault/internal/vector"
	"github.com/wesm/msgvault/internal/vector/sqlitevec"
)

// engineFixture wires a real sqlitevec backend to an in-memory corpus.
type engineFixture struct {
	Engine      *Engine
	Backend     *sqlitevec.Backend
	MainDB      *sql.DB
	GenID       vector.GenerationID
	Fingerprint string
}

// fakeEmbedder returns a deterministic vector pointing along axis 0.
type fakeEmbedder struct {
	dim int
}

func (f *fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, f.dim)
		v[0] = 1.0
		out[i] = v
	}
	return out, nil
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	mainDB, err := sql.Open("sqlite3", mainPath)
	if err != nil {
		t.Fatalf("open main: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })

	// sent_at is DATETIME (text) to match the production schema.
	schema := `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,
    subject TEXT,
    source_id INTEGER,
    sender_id INTEGER,
    has_attachments INTEGER DEFAULT 0,
    size_estimate INTEGER,
    sent_at DATETIME,
    deleted_at DATETIME,
    deleted_from_source_at DATETIME
);
CREATE TABLE message_bodies (
    message_id INTEGER PRIMARY KEY,
    body_text TEXT
);
CREATE VIRTUAL TABLE messages_fts USING fts5(subject, body, content='', contentless_delete=1);
CREATE TABLE message_labels (
    message_id INTEGER NOT NULL,
    label_id INTEGER NOT NULL,
    PRIMARY KEY (message_id, label_id)
);
CREATE TABLE message_recipients (
    id INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL,
    recipient_type TEXT NOT NULL,
    participant_id INTEGER NOT NULL
);`
	if _, err := mainDB.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	rows := []struct {
		id      int64
		subject string
		body    string
	}{
		{1, "meeting tomorrow", "Quarterly review at 10am."},
		{2, "lunch plans", "Tacos near Ferry Building."},
		{3, "travel itinerary", "Flight confirmation attached."},
	}
	for _, r := range rows {
		if _, err := mainDB.Exec(
			`INSERT INTO messages (id, subject) VALUES (?, ?)`, r.id, r.subject); err != nil {
			t.Fatalf("insert msg: %v", err)
		}
		if _, err := mainDB.Exec(
			`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, r.id, r.body); err != nil {
			t.Fatalf("insert body: %v", err)
		}
		if _, err := mainDB.Exec(
			`INSERT INTO messages_fts (rowid, subject, body) VALUES (?, ?, ?)`, r.id, r.subject, r.body); err != nil {
			t.Fatalf("insert fts: %v", err)
		}
	}

	vecPath := filepath.Join(dir, "vectors.db")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecPath,
		MainPath:  mainPath,
		Dimension: 4,
		MainDB:    mainDB,
	})
	if err != nil {
		t.Fatalf("sqlitevec.Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	gid, err := b.CreateGeneration(ctx, "fake-model", 4)
	if err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	chunks := []vector.Chunk{
		{MessageID: 1, Vector: unitVec(4, 0), SourceCharLen: 50},
		{MessageID: 2, Vector: unitVec(4, 1), SourceCharLen: 30},
		{MessageID: 3, Vector: unitVec(4, 2), SourceCharLen: 40},
	}
	if err := b.Upsert(ctx, gid, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := b.ActivateGeneration(ctx, gid); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	fp := "fake-model:4"
	eng := NewEngine(b, mainDB, &fakeEmbedder{dim: 4}, Config{
		ExpectedFingerprint: fp,
		RRFK:                60,
		KPerSignal:          10,
		SubjectBoost:        1.0,
	})
	return &engineFixture{
		Engine:      eng,
		Backend:     b,
		MainDB:      mainDB,
		GenID:       gid,
		Fingerprint: fp,
	}
}

func unitVec(dim, axis int) []float32 {
	v := make([]float32, dim)
	v[axis] = 1.0
	return v
}

func TestEngine_Hybrid_HappyPath(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)

	results, meta, err := f.Engine.Search(ctx, SearchRequest{
		Mode:     ModeHybrid,
		FreeText: "meeting",
		Limit:    5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("empty results")
	}
	if results[0].MessageID != 1 {
		t.Errorf("top = %d, want 1", results[0].MessageID)
	}
	if meta.Generation.ID != f.GenID {
		t.Errorf("meta.Generation.ID = %d, want %d", meta.Generation.ID, f.GenID)
	}
	if meta.ReturnedCount != len(results) {
		t.Errorf("ReturnedCount=%d, len(results)=%d", meta.ReturnedCount, len(results))
	}
}

func TestEngine_Vector_HappyPath(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)

	results, meta, err := f.Engine.Search(ctx, SearchRequest{
		Mode:     ModeVector,
		FreeText: "anything",
		Limit:    5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("empty results")
	}
	if results[0].MessageID != 1 {
		t.Errorf("top = %d, want 1", results[0].MessageID)
	}
	// Vector-mode hits carry VectorScore and BM25Score=NaN — the
	// FusedHit contract treats NaN as "absent from this signal" so
	// generic rendering code can skip the BM25 column rather than
	// showing a spurious zero.
	for _, r := range results {
		if !math.IsNaN(r.BM25Score) {
			t.Errorf("msg %d: BM25Score=%v, want NaN for vector-only hits", r.MessageID, r.BM25Score)
		}
		if math.IsNaN(r.VectorScore) {
			t.Errorf("msg %d: VectorScore=%v, want non-NaN", r.MessageID, r.VectorScore)
		}
	}
	_ = meta
}

func TestEngine_StaleIndexRejected(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)

	badEng := NewEngine(f.Backend, f.MainDB, &fakeEmbedder{dim: 4}, Config{
		ExpectedFingerprint: "other-model:4",
		RRFK:                60,
		KPerSignal:          10,
		SubjectBoost:        1.0,
	})
	_, _, err := badEng.Search(ctx, SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 5,
	})
	if !errors.Is(err, vector.ErrIndexStale) {
		t.Errorf("err = %v, want ErrIndexStale", err)
	}
}

func TestEngine_FTSMode_Rejected(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	_, _, err := f.Engine.Search(ctx, SearchRequest{
		Mode: ModeFTS, FreeText: "meeting", Limit: 5,
	})
	if err == nil {
		t.Error("expected error for mode=fts, got nil")
	}
}

func TestEngine_EmptyFreeText_Rejected(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	_, _, err := f.Engine.Search(ctx, SearchRequest{
		Mode: ModeHybrid, FreeText: "", Limit: 5,
	})
	if err == nil {
		t.Error("expected error for empty FreeText, got nil")
	}
}

func TestEngine_UnknownMode_Rejected(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	_, _, err := f.Engine.Search(ctx, SearchRequest{
		Mode: "bogus", FreeText: "x", Limit: 5,
	})
	if err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

// TestEngine_PoolSaturated_WhenLimitBelowK verifies the fix for a
// bug where PoolSaturated was derived from len(hits) >= KPerSignal.
// When Limit < KPerSignal, the returned hit count could never hit
// that threshold, so the engine incorrectly reported an unsaturated
// pool even when the BM25 branch had more than K candidates.
func TestEngine_PoolSaturated_WhenLimitBelowK(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)

	// Seed a batch of FTS-matching messages well above KPerSignal=2.
	for i := int64(100); i < 110; i++ {
		if _, err := f.MainDB.ExecContext(ctx,
			`INSERT INTO messages (id, subject) VALUES (?, ?)`, i, "meeting"); err != nil {
			t.Fatalf("insert msg %d: %v", i, err)
		}
		if _, err := f.MainDB.ExecContext(ctx,
			`INSERT INTO messages_fts (rowid, subject, body) VALUES (?, ?, ?)`,
			i, "meeting", "meeting meeting"); err != nil {
			t.Fatalf("insert fts %d: %v", i, err)
		}
		if err := f.Backend.Upsert(ctx, f.GenID, []vector.Chunk{{MessageID: i, Vector: unitVec(4, 0), SourceCharLen: 10}}); err != nil {
			t.Fatalf("upsert msg %d: %v", i, err)
		}
	}

	tightEng := NewEngine(f.Backend, f.MainDB, &fakeEmbedder{dim: 4}, Config{
		ExpectedFingerprint: f.Fingerprint,
		RRFK:                60,
		KPerSignal:          2, // cap is 2, corpus has many matches
		SubjectBoost:        1.0,
	})

	results, meta, err := tightEng.Search(ctx, SearchRequest{
		Mode:     ModeHybrid,
		FreeText: "meeting",
		Limit:    1, // intentionally below KPerSignal
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results)=%d, want 1 (Limit=1)", len(results))
	}
	if !meta.PoolSaturated {
		t.Errorf("PoolSaturated=false despite Limit(1) < KPerSignal(2) and abundant candidates")
	}
}

// TestEngine_NoGenerations_ReturnsNotEnabled verifies the Search
// error path after the active generation is retired and no building
// one exists: callers expect ErrNotEnabled via ResolveActive, so the
// API layer can 503 with "vector_not_enabled" instead of a generic
// ErrNoActiveGeneration.
func TestEngine_NoGenerations_ReturnsNotEnabled(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	if err := f.Backend.RetireGeneration(ctx, f.GenID); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	_, _, err := f.Engine.Search(ctx, SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 5,
	})
	if !errors.Is(err, vector.ErrNotEnabled) {
		t.Errorf("err = %v, want ErrNotEnabled", err)
	}
}

// TestEngine_EmbedTimeout_WrappedAsErrEmbeddingTimeout covers the
// HTTP timeout path: when the embed call returns
// context.DeadlineExceeded (request handler timeout fired before
// the embedder responded), Search must wrap the error with
// vector.ErrEmbeddingTimeout so the API/MCP error mappers can
// surface a 503 embedding_timeout instead of a generic 500.
func TestEngine_EmbedTimeout_WrappedAsErrEmbeddingTimeout(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	timingOutEng := NewEngine(f.Backend, f.MainDB, &timeoutEmbedder{}, Config{
		ExpectedFingerprint: f.Fingerprint,
		RRFK:                60,
		KPerSignal:          10,
	})

	_, _, err := timingOutEng.Search(ctx, SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 5,
	})
	if !errors.Is(err, vector.ErrEmbeddingTimeout) {
		t.Errorf("err = %v, want wrapped ErrEmbeddingTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, must also wrap context.DeadlineExceeded", err)
	}
}

// timeoutEmbedder always reports the request context's deadline-exceeded
// — simulating an embedder that didn't respond before the HTTP handler
// timeout fired.
type timeoutEmbedder struct{}

func (timeoutEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, context.DeadlineExceeded
}

// TestEngine_BuildingOnly_ReturnsBuilding covers the "no active yet,
// first build running" case. ResolveActiveForFingerprint must
// differentiate this from ErrNotEnabled so clients can distinguish
// "configure vector search" from "wait for build".
func TestEngine_BuildingOnly_ReturnsBuilding(t *testing.T) {
	ctx := context.Background()
	f := newEngineFixture(t)
	if err := f.Backend.RetireGeneration(ctx, f.GenID); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	// A new building generation must be present; CreateGeneration
	// writes one directly.
	if _, err := f.Backend.CreateGeneration(ctx, "fake-model", 4); err != nil {
		t.Fatalf("CreateGeneration: %v", err)
	}
	_, _, err := f.Engine.Search(ctx, SearchRequest{
		Mode: ModeHybrid, FreeText: "meeting", Limit: 5,
	})
	if !errors.Is(err, vector.ErrIndexBuilding) {
		t.Errorf("err = %v, want ErrIndexBuilding", err)
	}
}
