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

	_ "github.com/mattn/go-sqlite3" // SQLite driver for vectors.db metadata commands.
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/vector"
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
	PendingCount int64
}

func runEmbeddingsList(cmd *cobra.Command, _ []string) error {
	db, closeDB, err := openEmbeddingsMetadataDB()
	if err != nil {
		return err
	}
	defer closeDB()

	rows, err := listEmbeddingGenerations(cmd.Context(), db)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No embedding generations found.")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSTATE\tMODEL\tDIM\tMESSAGES\tPENDING\tFINGERPRINT\tSTARTED\tCOMPLETED\tACTIVATED")
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			row.ID,
			row.State,
			row.Model,
			row.Dimension,
			row.MessageCount,
			row.PendingCount,
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

	db, closeDB, err := openEmbeddingsMetadataDB()
	if err != nil {
		return err
	}
	defer closeDB()

	row, err := getEmbeddingGeneration(cmd.Context(), db, gen)
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
	if err := retireEmbeddingGeneration(cmd.Context(), db, gen, embeddingsRetireForceActive); err != nil {
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

	db, closeDB, err := openEmbeddingsMetadataDB()
	if err != nil {
		return err
	}
	defer closeDB()

	row, err := getEmbeddingGeneration(cmd.Context(), db, gen)
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
	if row.PendingCount > 0 && !embeddingsActivateForce {
		return fmt.Errorf("generation %d still has %d pending embedding rows; run `msgvault embeddings resume` or pass --force",
			gen, row.PendingCount)
	}
	if row.SeededAt == nil && !embeddingsActivateForce {
		return fmt.Errorf("generation %d has not finished seeding; run `msgvault embeddings resume` or pass --force",
			gen)
	}

	active, hasActive, err := activeEmbeddingGeneration(cmd.Context(), db)
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

	if err := activateEmbeddingGeneration(cmd.Context(), db, gen, embeddingsActivateForce); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Generation %d activated.\n", gen)
	return nil
}

func openEmbeddingsMetadataDB() (*sql.DB, func(), error) {
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
	db, err := sql.Open("sqlite3", sqliteDSNWithBusyTimeout(vecPath))
	if err != nil {
		return nil, nil, fmt.Errorf("open vectors.db: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
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

func listEmbeddingGenerations(ctx context.Context, db *sql.DB) ([]embeddingGenerationRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.id, g.model, g.dimension, g.fingerprint, g.state,
		       g.started_at, g.completed_at, g.activated_at, g.message_count,
		       g.seeded_at, COUNT(p.message_id) AS pending_count
		  FROM index_generations g
		  LEFT JOIN pending_embeddings p ON p.generation_id = g.id
		 GROUP BY g.id
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

func getEmbeddingGeneration(ctx context.Context, db *sql.DB, gen vector.GenerationID) (embeddingGenerationRow, error) {
	row := db.QueryRowContext(ctx, `
		SELECT g.id, g.model, g.dimension, g.fingerprint, g.state,
		       g.started_at, g.completed_at, g.activated_at, g.message_count,
		       g.seeded_at, COUNT(p.message_id) AS pending_count
		  FROM index_generations g
		  LEFT JOIN pending_embeddings p ON p.generation_id = g.id
		 WHERE g.id = ?
		 GROUP BY g.id`, int64(gen))
	g, err := scanEmbeddingGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingGenerationRow{}, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return embeddingGenerationRow{}, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	return g, nil
}

func activeEmbeddingGeneration(ctx context.Context, db *sql.DB) (embeddingGenerationRow, bool, error) {
	row := db.QueryRowContext(ctx, `
		SELECT g.id, g.model, g.dimension, g.fingerprint, g.state,
		       g.started_at, g.completed_at, g.activated_at, g.message_count,
		       g.seeded_at, COUNT(p.message_id) AS pending_count
		  FROM index_generations g
		  LEFT JOIN pending_embeddings p ON p.generation_id = g.id
		 WHERE g.state = ?
		 GROUP BY g.id`, string(vector.GenerationActive))
	g, err := scanEmbeddingGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingGenerationRow{}, false, nil
	}
	if err != nil {
		return embeddingGenerationRow{}, false, fmt.Errorf("lookup active generation: %w", err)
	}
	return g, true, nil
}

func retireEmbeddingGeneration(ctx context.Context, db *sql.DB, gen vector.GenerationID, forceActive bool) error {
	stateFilter := `state = ?`
	args := []any{string(vector.GenerationBuilding), int64(gen)}
	if forceActive {
		stateFilter = `state IN (?, ?)`
		args = []any{string(vector.GenerationBuilding), string(vector.GenerationActive), int64(gen)}
	}
	res, err := db.ExecContext(ctx,
		`UPDATE index_generations SET state = 'retired' WHERE `+stateFilter+` AND id = ?`, args...)
	if err != nil {
		return fmt.Errorf("retire generation %d: %w", gen, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}

	row, err := getEmbeddingGeneration(ctx, db, gen)
	if err != nil {
		return err
	}
	if row.State == vector.GenerationRetired {
		return nil
	}
	if row.State == vector.GenerationActive && !forceActive {
		return fmt.Errorf("generation %d is active; pass --force-active to retire the serving generation", gen)
	}
	return fmt.Errorf("generation %d could not be retired from %q state", gen, row.State)
}

func activateEmbeddingGeneration(ctx context.Context, db *sql.DB, gen vector.GenerationID, force bool) error {
	now := time.Now().Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activate transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		 SET state = 'retired', completed_at = COALESCE(completed_at, ?)
		 WHERE state = 'active'`, now); err != nil {
		return fmt.Errorf("retire previous active: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE index_generations
		 SET state = 'active', activated_at = ?, completed_at = COALESCE(completed_at, ?)
		 WHERE id = ? AND state = 'building'
		   AND (? OR seeded_at IS NOT NULL)
		   AND (? OR NOT EXISTS (
		       SELECT 1 FROM pending_embeddings WHERE generation_id = ?
		   ))`, now, now, int64(gen), force, force, int64(gen))
	if err != nil {
		return fmt.Errorf("activate generation %d: %w", gen, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var pending int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&pending); err != nil {
			return fmt.Errorf("count pending rows for generation %d: %w", gen, err)
		}
		if pending > 0 && !force {
			return fmt.Errorf("generation %d still has %d pending embedding rows; run `msgvault embeddings resume` or pass --force",
				gen, pending)
		}
		var state vector.GenerationState
		var seededAt sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT state, seeded_at FROM index_generations WHERE id = ?`, int64(gen)).Scan(&state, &seededAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
			}
			return fmt.Errorf("lookup generation %d: %w", gen, err)
		}
		if state == vector.GenerationBuilding && !seededAt.Valid && !force {
			return fmt.Errorf("generation %d has not finished seeding; run `msgvault embeddings resume` or pass --force",
				gen)
		}
		return fmt.Errorf("generation %d not in %q state", gen, vector.GenerationBuilding)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activate transaction: %w", err)
	}
	return nil
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
		&row.PendingCount,
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
