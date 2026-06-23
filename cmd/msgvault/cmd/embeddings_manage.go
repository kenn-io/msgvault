package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for PostgreSQL metadata commands.
	_ "github.com/mattn/go-sqlite3"    // SQLite driver for vectors.db metadata commands.
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type embeddingGenerationRow struct {
	ID           vector.GenerationID
	Model        string
	Dimension    int
	Fingerprint  string
	State        vector.GenerationState
	StartedAt    time.Time
	SeededAt     *time.Time
	CompletedAt  *time.Time
	ActivatedAt  *time.Time
	MessageCount int64
	// Coverage counts for this generation over the live-message universe,
	// computed from the main DB (live/stamped/missing) plus the vector
	// backend (embedded). Filled by fillCoverage / fillFullCoverage. The
	// invariant LiveCount == EmbeddedCount + BlankCount + MissingCount holds.
	//
	//   - LiveCount:     total live messages (the embedding universe).
	//   - EmbeddedCount: live messages that actually have >=1 vector for
	//     this generation (COUNT(DISTINCT message_id) in the embeddings
	//     table). Only filled by fillFullCoverage (needs the backend).
	//   - BlankCount:    stamped-but-empty messages (stamped embed_gen=id
	//     but no vector) — the body-extraction-regression detector.
	//     Only filled by fillFullCoverage.
	//   - MissingCount:  live messages not yet stamped for this generation.
	LiveCount     int64
	EmbeddedCount int64
	BlankCount    int64
	MissingCount  int64
}

// fillCoverage populates row.LiveCount and row.MissingCount from the main
// DB so the management commands can gate on how many live messages still
// need embedding for the generation. This is the cheap, backend-free path
// used by the activation gate (which only needs MissingCount). It leaves
// EmbeddedCount/BlankCount at zero — use fillFullCoverage for the display
// table where the embedded/blank split is wanted. A failure is surfaced to
// the caller.
func fillCoverage(ctx context.Context, row *embeddingGenerationRow) error {
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open main db for coverage: %w", err)
	}
	defer func() { _ = s.Close() }()
	live, _, _, missing, err := s.CoverageCounts(ctx, int64(row.ID))
	if err != nil {
		return err
	}
	row.LiveCount = live
	row.MissingCount = missing
	return nil
}

// fillFullCoverage populates the complete live/embedded/blank/missing split
// for the generation. The main DB supplies live, stamped (embed_gen=id),
// and missing; the vector backend supplies embedded (COUNT(DISTINCT
// message_id) in the embeddings table for this generation). blank is the
// remainder, stamped - embedded, clamped >= 0 — messages stamped terminal
// DONE but with no vector (the empty/unembeddable case). The invariant
// live == embedded + blank + missing holds. The backend handle is passed
// in by the caller (which already opened it for the generation listing).
func fillFullCoverage(ctx context.Context, backend vector.Backend, row *embeddingGenerationRow) error {
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open main db for coverage: %w", err)
	}
	defer func() { _ = s.Close() }()
	live, stamped, _, missing, err := s.CoverageCounts(ctx, int64(row.ID))
	if err != nil {
		return err
	}
	embedded, err := backend.EmbeddedMessageCount(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("count embedded messages for generation %d: %w", row.ID, err)
	}
	blank := max(stamped-embedded, 0)
	row.LiveCount = live
	row.EmbeddedCount = embedded
	row.BlankCount = blank
	row.MissingCount = missing
	return nil
}

// ensureMainSchema opens the main DB and runs InitSchema so that an
// upgraded SQLite archive (whose messages table predates the embed_gen
// column) gets the column added before any management command reads
// embed_gen via CoverageCounts. Mirrors the serve.go / runEmbed pattern.
// Cheap and idempotent on an already-current schema; harmless on PG.
func ensureMainSchema() error {
	s, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return fmt.Errorf("open main db: %w", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	return nil
}

func runEmbeddingsList(cmd *cobra.Command, _ []string) error {
	if err := ensureMainSchema(); err != nil {
		return err
	}
	db, rebind, closeDB, err := openEmbeddingsMetadataDB(cmd.Context())
	if err != nil {
		return err
	}
	defer closeDB()

	rows, err := listEmbeddingGenerations(cmd.Context(), db, rebind)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No embedding generations found.")
		return nil
	}

	// Fill per-generation coverage (live/embedded/blank/missing) for
	// non-retired generations — the interesting numbers. The embedded leg
	// comes from the vector backend (the embeddings table), so open it once
	// and thread it down. Retired generations are immutable; leave their
	// coverage at zero and skip the backend scan.
	needCoverage := false
	for i := range rows {
		if rows[i].State != vector.GenerationRetired {
			needCoverage = true
			break
		}
	}
	if needCoverage {
		backend, closeBackend, err := openEmbeddingsBackend(cmd.Context())
		if err != nil {
			return err
		}
		defer closeBackend()
		for i := range rows {
			if rows[i].State == vector.GenerationRetired {
				continue
			}
			if err := fillFullCoverage(cmd.Context(), backend, &rows[i]); err != nil {
				return err
			}
		}
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATE\tMODEL\tDIM\tLIVE\tEMBEDDED\tBLANK\tMISSING\tFINGERPRINT\tSTARTED\tCOMPLETED\tACTIVATED")
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			row.ID,
			row.State,
			row.Model,
			row.Dimension,
			row.LiveCount,
			row.EmbeddedCount,
			row.BlankCount,
			row.MissingCount,
			row.Fingerprint,
			formatGenerationTime(row.StartedAt),
			formatGenerationTimePtr(row.CompletedAt),
			formatGenerationTimePtr(row.ActivatedAt),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush embedding generations table: %w", err)
	}
	return nil
}

func runEmbeddingsRetire(cmd *cobra.Command, args []string) error {
	gen, err := parseGenerationID(args[0])
	if err != nil {
		return err
	}
	if err := ensureMainSchema(); err != nil {
		return err
	}

	db, rebind, closeDB, err := openEmbeddingsMetadataDB(cmd.Context())
	if err != nil {
		return err
	}
	defer closeDB()

	row, err := getEmbeddingGeneration(cmd.Context(), db, rebind, gen)
	if err != nil {
		return err
	}
	switch row.State {
	case vector.GenerationRetired:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d is already retired.\n", gen)
		return nil
	case vector.GenerationBuilding:
	case vector.GenerationActive:
		if !embeddingsRetireForceActive {
			return fmt.Errorf("generation %d is active; pass --force-active to retire the serving generation", gen)
		}
	}

	if !embeddingsRetireYes {
		prompt := fmt.Sprintf("Retire generation %d (%s)? ", gen, row.Fingerprint)
		if !confirmEmbed(cmd, prompt) {
			return errors.New("aborted")
		}
	}

	// Route the state transition through the vector backend so the
	// delete-on-retire invariant lives in one place (pgvector deletes the
	// retired generation's embeddings; sqlitevec retains them). The
	// active-gen preflight above is a friendly fast-fail, but the backend's
	// RetireGeneration enforces the same guard ATOMICALLY inside the retire
	// transaction: when force is false it refuses to retire a generation that
	// is state='active' (returning vector.ErrRefuseRetireActive) WITHOUT
	// deleting embeddings — so a concurrent activation between the preflight
	// read and this call cannot delete the now-serving generation's
	// embeddings. We pass --force-active as force to bypass the gate.
	backend, closeBackend, err := openEmbeddingsBackend(cmd.Context())
	if err != nil {
		return err
	}
	defer closeBackend()
	if err := backend.RetireGeneration(cmd.Context(), gen, embeddingsRetireForceActive); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d retired.\n", gen)
	return nil
}

func runEmbeddingsActivate(cmd *cobra.Command, args []string) error {
	gen, err := parseGenerationID(args[0])
	if err != nil {
		return err
	}
	if err := ensureMainSchema(); err != nil {
		return err
	}

	db, rebind, closeDB, err := openEmbeddingsMetadataDB(cmd.Context())
	if err != nil {
		return err
	}
	defer closeDB()

	row, err := getEmbeddingGeneration(cmd.Context(), db, rebind, gen)
	if err != nil {
		return err
	}
	if row.State != vector.GenerationBuilding {
		return fmt.Errorf("generation %d is %q, not %q", gen, row.State, vector.GenerationBuilding)
	}
	expected := cfg.Vector.GenerationFingerprint()
	if row.Fingerprint != expected && !embeddingsActivateForce {
		return fmt.Errorf("generation %d fingerprint=%q does not match config=%q; pass --force to activate anyway",
			gen, row.Fingerprint, expected)
	}
	// The coverage gate is enforced inside backend.ActivateGeneration
	// (atomically on PG; via a Go pre-check on SQLite). We still surface a
	// friendly pre-flight error here (against the main-DB coverage) so the
	// common case fails fast before opening a backend connection and before
	// prompting — but the backend's gate is the authoritative guarantee.
	if !embeddingsActivateForce {
		if err := fillCoverage(cmd.Context(), &row); err != nil {
			return err
		}
		if row.MissingCount > 0 {
			return fmt.Errorf("generation %d still has %d message(s) needing embedding; run `msgvault embeddings resume` or pass --force",
				gen, row.MissingCount)
		}
	}

	active, hasActive, err := activeEmbeddingGeneration(cmd.Context(), db, rebind)
	if err != nil {
		return err
	}
	if !embeddingsActivateYes {
		prompt := fmt.Sprintf("Activate generation %d (%s)", gen, row.Fingerprint)
		if hasActive {
			prompt += fmt.Sprintf(" and retire active generation %d (%s)", active.ID, active.Fingerprint)
		}
		prompt += "? "
		if !confirmEmbed(cmd, prompt) {
			return errors.New("aborted")
		}
	}

	// Route through the vector backend so the auto-retire of the previously
	// active generation deletes its embeddings on PG (the same delete-on-retire
	// invariant as the retire path). The backend's ActivateGeneration requires
	// the target to be in 'building' state, enforces the coverage (no-missing)
	// gate ATOMICALLY with the state flip (unless force), and auto-retires the
	// prior active generation in one transaction. The fingerprint check above
	// is the only gate the backend cannot make (it does not know the config
	// fingerprint); the coverage gate is owned by the backend.
	backend, closeBackend, err := openEmbeddingsBackend(cmd.Context())
	if err != nil {
		return err
	}
	defer closeBackend()
	if err := backend.ActivateGeneration(cmd.Context(), gen, embeddingsActivateForce); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d activated.\n", gen)
	return nil
}

// openEmbeddingsMetadataDB opens the database that holds embedding generation
// metadata and returns a handle, a rebind function for SQL placeholders, a
// close callback, and any error.
//
// On PostgreSQL deployments the embedding tables live in the main Postgres
// database alongside messages — there is no separate vectors.db. On SQLite
// deployments the metadata lives in vectors.db as before.
//
// rebind converts ? placeholders to $1, $2, … for PostgreSQL; it is the
// identity function for SQLite so all query helpers can use it unconditionally.
func openEmbeddingsMetadataDB(ctx context.Context) (*sql.DB, func(string) string, func(), error) {
	dsn := cfg.DatabaseDSN()
	if store.IsPostgresURL(dsn) {
		// Use the store-level PG opener so that connection runtime params
		// (statement_timeout) and the pgx stdlib registration are applied
		// consistently with the rest of the codebase. Raw sql.Open("pgx",
		// dsn) bypasses those settings.
		db, cleanup, err := store.OpenPostgresDB(dsn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open postgres for embeddings metadata: %w", err)
		}
		closeDB := func() { _ = db.Close(); cleanup() }
		// Pre-check that the embedding metadata tables exist. They are created
		// only by pgvector.Migrate (on an embed/serve run), not by the core PG
		// store init, so on a PG deployment where no embed run has happened yet
		// the bare query path would surface a raw
		// `relation "index_generations" does not exist (SQLSTATE 42P01)`.
		// Return a friendly message mirroring the SQLite "vectors.db not found"
		// UX and pointing at `msgvault embeddings build`.
		var reg sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('index_generations')`).Scan(&reg); err != nil {
			closeDB()
			return nil, nil, nil, fmt.Errorf("check embeddings metadata: %w", err)
		}
		if !reg.Valid {
			closeDB()
			return nil, nil, nil, errors.New(
				"no embedding metadata found in PostgreSQL; run \"msgvault embeddings build\" first")
		}
		rebind := (&store.PostgreSQLDialect{}).Rebind
		return db, rebind, closeDB, nil
	}

	vecPath := cfg.Vector.DBPath
	if vecPath == "" {
		vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	if _, err := os.Stat(vecPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, fmt.Errorf("vectors.db not found at %s", vecPath)
		}
		return nil, nil, nil, fmt.Errorf("stat vectors.db: %w", err)
	}
	db, err := sql.Open("sqlite3", sqliteDSNWithBusyTimeout(vecPath))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open vectors.db: %w", err)
	}
	rebind := (&store.SQLiteDialect{}).Rebind
	return db, rebind, func() { _ = db.Close() }, nil
}

// openEmbeddingsBackend constructs the vector backend for the active dialect,
// mirroring how embed_vector.go builds it. The CLI retire/activate commands
// route their state transitions through the backend so a SINGLE implementation
// owns the delete-on-retire invariant (pgvector deletes a retired generation's
// embeddings so the shared HNSW graph stays generation-clean; sqlitevec retains
// them because its vec0 PARTITION KEY isolates retired rows). Raw-SQL helpers
// that only flip index_generations.state would bypass that invariant on PG.
//
// Returns the backend and a close callback. On a build without the relevant
// vector tag the package stubs' Open returns ErrNotBuilt.
func openEmbeddingsBackend(ctx context.Context) (vector.Backend, func(), error) {
	dsn := cfg.DatabaseDSN()
	if store.IsPostgresURL(dsn) {
		db, cleanup, err := store.OpenPostgresDB(dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("open postgres for embeddings backend: %w", err)
		}
		// SkipMigrate skips only the privileged CREATE EXTENSION + full
		// migrate: the extension + metadata tables already exist (the caller's
		// openEmbeddingsMetadataDB pre-checks index_generations), so a
		// management command must not attempt the privileged extension step.
		// This open is WRITABLE management, NOT read-only — ReadOnly stays
		// false so Open still applies the extension-less schema (bringing up
		// embed_watermark etc. if missing) and runs the one-time embed_gen
		// upgrade backfill, matching the SQLite management path (which always
		// migrates vectors.db + backfills). Without this, a post-upgrade PG
		// archive would report its whole corpus as missing on the first
		// writable management command.
		b, err := pgvector.Open(ctx, pgvector.Options{
			DB:          db,
			Dimension:   cfg.Vector.Embeddings.Dimension,
			SkipMigrate: true,
		})
		if err != nil {
			_ = db.Close()
			cleanup()
			return nil, nil, fmt.Errorf("open pgvector backend: %w", err)
		}
		return b, func() { _ = b.Close(); _ = db.Close(); cleanup() }, nil
	}

	if err := sqlitevec.RegisterExtension(); err != nil {
		return nil, nil, fmt.Errorf("register sqlite-vec: %w", err)
	}
	vecPath := cfg.Vector.DBPath
	if vecPath == "" {
		vecPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
	}
	if _, err := os.Stat(vecPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("vectors.db not found at %s", vecPath)
		}
		return nil, nil, fmt.Errorf("stat vectors.db: %w", err)
	}
	// On SQLite the messages table (and embed_gen) lives in the main DB,
	// in a SEPARATE file from vectors.db. Backend methods that gate on
	// live-message coverage — ActivateGeneration's hasMissingForGen, and
	// the live-intersected EmbeddedMessageCount — dereference b.mainDB, so
	// the management path must open and pass a main-DB handle just like
	// embed_vector.go does. Omitting it leaves b.mainDB nil and panics on
	// `msgvault embeddings activate`. Close it in the returned cleanup.
	mainStore, err := store.Open(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open main db for embeddings backend: %w", err)
	}
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      vecPath,
		MainPath:  dsn,
		Dimension: cfg.Vector.Embeddings.Dimension,
		MainDB:    mainStore.DB(),
	})
	if err != nil {
		_ = mainStore.Close()
		return nil, nil, fmt.Errorf("open vectors.db backend: %w", err)
	}
	return b, func() { _ = b.Close(); _ = mainStore.Close() }, nil
}

func sqliteDSNWithBusyTimeout(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_busy_timeout=5000"
}

func parseGenerationID(s string) (vector.GenerationID, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid generation id %q", s)
	}
	return vector.GenerationID(id), nil
}

//nolint:unparam // rebind is a no-op here (no ? placeholders) but kept for signature symmetry with the other embedding-generation query helpers and their shared call sites
func listEmbeddingGenerations(ctx context.Context, db *sql.DB, rebind func(string) string) ([]embeddingGenerationRow, error) {
	// No ? placeholders in this query; rebind is a no-op here but kept for
	// symmetry so all helpers share the same signature.
	rows, err := db.QueryContext(ctx, `
		SELECT g.id, g.model, g.dimension, g.fingerprint, g.state,
		       g.started_at, g.completed_at, g.activated_at, g.message_count,
		       g.seeded_at
		  FROM index_generations g
		 ORDER BY g.id`)
	if err != nil {
		return nil, fmt.Errorf("list embedding generations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []embeddingGenerationRow
	for rows.Next() {
		row, err := scanEmbeddingGeneration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list embedding generations: %w", err)
	}
	return out, nil
}

func getEmbeddingGeneration(ctx context.Context, db *sql.DB, rebind func(string) string, gen vector.GenerationID) (embeddingGenerationRow, error) {
	row := db.QueryRowContext(ctx, rebind(`
		SELECT g.id, g.model, g.dimension, g.fingerprint, g.state,
		       g.started_at, g.completed_at, g.activated_at, g.message_count,
		       g.seeded_at
		  FROM index_generations g
		 WHERE g.id = ?`), int64(gen))
	g, err := scanEmbeddingGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingGenerationRow{}, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return embeddingGenerationRow{}, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	return g, nil
}

func activeEmbeddingGeneration(ctx context.Context, db *sql.DB, rebind func(string) string) (embeddingGenerationRow, bool, error) {
	row := db.QueryRowContext(ctx, rebind(`
		SELECT g.id, g.model, g.dimension, g.fingerprint, g.state,
		       g.started_at, g.completed_at, g.activated_at, g.message_count,
		       g.seeded_at
		  FROM index_generations g
		 WHERE g.state = ?`), string(vector.GenerationActive))
	g, err := scanEmbeddingGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingGenerationRow{}, false, nil
	}
	if err != nil {
		return embeddingGenerationRow{}, false, fmt.Errorf("lookup active generation: %w", err)
	}
	return g, true, nil
}

type generationScanner interface {
	Scan(dest ...any) error
}

func scanEmbeddingGeneration(s generationScanner) (embeddingGenerationRow, error) {
	var row embeddingGenerationRow
	var startedAt int64
	var seededAt, completedAt, activatedAt sql.NullInt64
	if err := s.Scan(
		&row.ID,
		&row.Model,
		&row.Dimension,
		&row.Fingerprint,
		&row.State,
		&startedAt,
		&completedAt,
		&activatedAt,
		&row.MessageCount,
		&seededAt,
	); err != nil {
		return embeddingGenerationRow{}, err
	}
	row.StartedAt = time.Unix(startedAt, 0)
	if seededAt.Valid {
		t := time.Unix(seededAt.Int64, 0)
		row.SeededAt = &t
	}
	if completedAt.Valid {
		t := time.Unix(completedAt.Int64, 0)
		row.CompletedAt = &t
	}
	if activatedAt.Valid {
		t := time.Unix(activatedAt.Int64, 0)
		row.ActivatedAt = &t
	}
	return row, nil
}

func formatGenerationTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatGenerationTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return formatGenerationTime(*t)
}
