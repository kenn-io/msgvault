//go:build sqlite_vec || pgvector

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// runHybridSearch executes a vector or hybrid search against the local
// msgvault archive using the sqlite-vec backend and configured embedding
// endpoint. Invoked from search.go when --mode is "vector" or "hybrid".
// scope carries any resolved --account/--collection scope; an empty
// Scope means no scope flag was supplied.
func runHybridSearch(cmd *cobra.Command, queryStr, mode string, explain bool, scope Scope) error {
	if queryStr == "" {
		return errors.New("empty search query")
	}
	if !cfg.Vector.Enabled {
		return errors.New("vector search not enabled; set [vector].enabled = true in config")
	}
	if cfg.Vector.Embeddings.Endpoint == "" || cfg.Vector.Embeddings.Model == "" {
		return errors.New("vector search requires [vector.embeddings] endpoint and model in config")
	}

	ctx := cmd.Context()

	dsn := cfg.DatabaseDSN()

	// Resolve the dialect from the DSN so the participant/label lookups in
	// BuildFilter and the IN-clause in hydrateHybridResults run with the
	// driver's native placeholders. SQLite's Rebind is identity, so the
	// SQLite path below is byte-identical to before.
	var dialect store.Dialect = &store.SQLiteDialect{}
	if store.IsPostgresURL(dsn) {
		dialect = &store.PostgreSQLDialect{}
	}

	var (
		mainDB  *sql.DB
		backend vector.Backend
		closeFn func() error
	)
	if store.IsPostgresURL(dsn) {
		// PostgreSQL: pgvector embeddings live in the SAME database as
		// messages, so there is no separate vectors.db. Open the main
		// store (which registers the pgx driver and sets statement_timeout
		// via pgx RuntimeParams during connect; search_path is taken from
		// the DSN if present — InitConn is a no-op) and share its handle
		// with the pgvector backend.
		st, err := store.Open(dsn)
		if err != nil {
			return fmt.Errorf("open main db: %w", err)
		}
		mainDB = st.DB()
		// store.Open returns a WRITABLE connection (not OpenReadOnly), so
		// ReadOnly stays false: Open runs the full migrate + one-time upgrade
		// backfill, exactly as before. This CLI search path is user-facing
		// read-only in intent but the connection can write, so it correctly
		// behaves like serve's writable open.
		pgb, err := pgvector.Open(ctx, pgvector.Options{
			DB:            mainDB,
			Dimension:     cfg.Vector.Embeddings.Dimension,
			BuildScope:    cfg.Vector.Embed.Scope.BuildScope(),
			SkipExtension: cfg.Vector.SkipExtensionCreate,
		})
		if err != nil {
			_ = st.Close()
			return fmt.Errorf("open pgvector backend: %w", err)
		}
		backend = pgb
		// pgvector.Close is a no-op (the store owns the handle); close the
		// store to release the pool.
		closeFn = func() error {
			_ = pgb.Close()
			return st.Close()
		}
	} else {
		var err error
		mainDB, err = sql.Open("sqlite3", dsn)
		if err != nil {
			return fmt.Errorf("open main db: %w", err)
		}

		vecDBPath := cfg.Vector.DBPath
		if vecDBPath == "" {
			vecDBPath = filepath.Join(cfg.Data.DataDir, "vectors.db")
		}

		if err := sqlitevec.RegisterExtension(); err != nil {
			_ = mainDB.Close()
			return fmt.Errorf("register sqlite-vec: %w", err)
		}

		sb, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path:       vecDBPath,
			MainPath:   dsn,
			Dimension:  cfg.Vector.Embeddings.Dimension,
			MainDB:     mainDB,
			BuildScope: cfg.Vector.Embed.Scope.BuildScope(),
		})
		if err != nil {
			_ = mainDB.Close()
			return fmt.Errorf("open vectors.db: %w", err)
		}
		backend = sb
		closeFn = func() error {
			_ = sb.Close()
			return mainDB.Close()
		}
	}
	defer func() { _ = closeFn() }()

	active, err := vector.ResolveActiveForFingerprint(ctx, backend, cfg.Vector.GenerationFingerprint())
	if err != nil {
		return fmt.Errorf("resolve active generation: %w", err)
	}

	embedClient := embed.NewClient(embed.Config{
		Endpoint:   cfg.Vector.Embeddings.Endpoint,
		APIKey:     cfg.Vector.Embeddings.APIKey(),
		Model:      cfg.Vector.Embeddings.Model,
		Dimension:  cfg.Vector.Embeddings.Dimension,
		Timeout:    cfg.Vector.Embeddings.Timeout,
		MaxRetries: cfg.Vector.Embeddings.MaxRetries,
	})

	eng := hybrid.NewEngine(backend, mainDB, embedClient, hybrid.Config{
		ExpectedFingerprint: cfg.Vector.GenerationFingerprint(),
		RRFK:                cfg.Vector.Search.RRFK,
		KPerSignal:          cfg.Vector.Search.KPerSignal,
		SubjectBoost:        cfg.Vector.Search.SubjectBoost,
		Rebind:              dialect.Rebind,
		BuildScope:          cfg.Vector.Embed.Scope.BuildScope(),
	})

	q := search.Parse(queryStr)

	filter, err := hybrid.BuildFilter(ctx, mainDB, dialect.Rebind, q)
	if err != nil {
		return fmt.Errorf("build filter: %w", err)
	}

	// Apply resolved --account/--collection scope so vector and hybrid
	// modes honour the same scope as FTS. Earlier this branch only
	// looked at --account directly and silently ignored --collection.
	if !scope.IsEmpty() {
		filter.SourceIDs = scope.SourceIDs()
		if scope.IsCollection() {
			n := len(filter.SourceIDs)
			suffix := "s"
			if n == 1 {
				suffix = ""
			}
			fmt.Fprintf(os.Stderr,
				"Searching collection %q (%d account%s)\n",
				scope.DisplayName(), n, suffix,
			)
		}
	}

	freeText := strings.Join(q.TextTerms, " ")

	subjectTerms := make([]string, 0, len(q.TextTerms))
	for _, t := range q.TextTerms {
		subjectTerms = append(subjectTerms, strings.ToLower(t))
	}

	req := hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     freeText,
		Filter:       filter,
		Limit:        searchLimit,
		SubjectTerms: subjectTerms,
		Explain:      explain,
	}

	logger.Info("vector search start",
		"mode", mode,
		"query_len", len(queryStr),
		"limit", searchLimit,
		"explain", explain,
		"generation_id", int64(active.ID),
	)
	started := time.Now()

	hits, meta, err := eng.Search(ctx, req)
	if err != nil {
		logger.Warn("vector search failed",
			"mode", mode,
			"duration_ms", time.Since(started).Milliseconds(),
			"error", err.Error(),
		)
		return fmt.Errorf("hybrid search: %w", err)
	}
	logger.Info("vector search done",
		"mode", mode,
		"results", len(hits),
		"duration_ms", time.Since(started).Milliseconds(),
	)

	results, err := hydrateHybridResults(ctx, mainDB, dialect.Rebind, hits)
	if err != nil {
		return fmt.Errorf("hydrate results: %w", err)
	}

	if searchJSON {
		return outputHybridResultsJSON(results, meta, explain)
	}
	return outputHybridResultsTable(results, meta, explain)
}

type hybridResultRow struct {
	MessageID      int64
	Subject        string
	FromEmail      string
	SentAt         time.Time
	RRFScore       float64
	BM25Score      float64
	VectorScore    float64
	SubjectBoosted bool
}

// hydrateHybridResults fetches subject, sender, and sent_at for each
// hit from the main DB, preserving the RRF ordering of the input hits.
//
// rebind converts the IN-clause ? placeholders to the driver's native
// form. On PostgreSQL pgx rejects bare ?, so the caller must pass the
// dialect's Rebind; nil falls back to identity (SQLite leaves the
// placeholders unchanged).
func hydrateHybridResults(ctx context.Context, db *sql.DB, rebind func(string) string, hits []vector.FusedHit) ([]hybridResultRow, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	if rebind == nil {
		rebind = func(s string) string { return s }
	}
	placeholders := make([]string, len(hits))
	args := make([]any, len(hits))
	orderIdx := make(map[int64]int, len(hits))
	for i, h := range hits {
		placeholders[i] = "?"
		args[i] = h.MessageID
		orderIdx[h.MessageID] = i
	}
	// Liveness is enforced upstream in the sqlite-vec backend's filter
	// CTE used for ranking; re-filtering here would silently drop hits
	// whose row was soft-deleted between ranking and hydration,
	// returning a result list shorter than the ranked hits. Hydrate
	// whatever was ranked.
	//
	// Sender hydration: email messages store the sender in
	// message_recipients (recipient_type='from'); chat/SMS messages
	// store it via messages.sender_id. COALESCE the subquery result
	// first so that the message_recipients path wins for email
	// (matching the main query engine's behaviour) and falls back to
	// sender_id for chat/SMS sources.
	q := rebind(fmt.Sprintf(`
		SELECT m.id, COALESCE(m.subject,''), COALESCE(p.email_address,''), m.sent_at
		FROM messages m
		LEFT JOIN participants p ON p.id = COALESCE(
			(SELECT mr.participant_id FROM message_recipients mr
			 WHERE mr.message_id = m.id AND mr.recipient_type = 'from'
			 ORDER BY mr.id LIMIT 1),
			m.sender_id
		)
		WHERE m.id IN (%s)`, strings.Join(placeholders, ",")))
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// hits ranked by the vector engine may have been soft-deleted between
	// ranking and hydration. Track which slots got filled so we can drop
	// the empty ones and warn about the gap.
	filled := make([]bool, len(hits))
	out := make([]hybridResultRow, len(hits))
	for rows.Next() {
		var id int64
		var subject, from string
		var sentAt sql.NullTime
		if err := rows.Scan(&id, &subject, &from, &sentAt); err != nil {
			return nil, fmt.Errorf("scan message row: %w", err)
		}
		idx, ok := orderIdx[id]
		if !ok {
			continue
		}
		h := hits[idx]
		row := hybridResultRow{
			MessageID:      id,
			Subject:        subject,
			FromEmail:      from,
			RRFScore:       h.RRFScore,
			BM25Score:      h.BM25Score,
			VectorScore:    h.VectorScore,
			SubjectBoosted: h.SubjectBoosted,
		}
		if sentAt.Valid {
			row.SentAt = sentAt.Time
		}
		out[idx] = row
		filled[idx] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	dropped := 0
	compact := out[:0]
	for i, ok := range filled {
		if ok {
			compact = append(compact, out[i])
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		logger.Warn("hydration dropped hits (likely soft-deleted between rank and hydrate)",
			"dropped", dropped, "ranked", len(hits))
	}
	return compact, nil
}

func outputHybridResultsTable(results []hybridResultRow, meta hybrid.ResultMeta, explain bool) error {
	if len(results) == 0 {
		fmt.Println("No messages found.")
		fmt.Printf("\nGeneration #%d (%s, fingerprint=%q)\n",
			int64(meta.Generation.ID), meta.Generation.State, meta.Generation.Fingerprint)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if explain {
		_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT\tRRF\tBM25\tVEC")
		_, _ = fmt.Fprintln(w, "──\t────\t────\t───────\t───\t────\t───")
	} else {
		_, _ = fmt.Fprintln(w, "ID\tDATE\tFROM\tSUBJECT")
		_, _ = fmt.Fprintln(w, "──\t────\t────\t───────")
	}
	for _, r := range results {
		date := r.SentAt.Format("2006-01-02")
		from := truncate(r.FromEmail, 30)
		subject := truncate(r.Subject, 50)
		if r.SubjectBoosted {
			subject += " *"
		}
		if explain {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.MessageID, date, from, subject, formatOptionalScore(r.RRFScore),
				formatOptionalScore(r.BM25Score), formatOptionalScore(r.VectorScore))
		} else {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
				r.MessageID, date, from, subject)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush table output: %w", err)
	}
	fmt.Printf("\nShowing %d results (generation #%d %s, fingerprint=%q)\n",
		len(results), int64(meta.Generation.ID), meta.Generation.State, meta.Generation.Fingerprint)
	return nil
}

func outputHybridResultsJSON(results []hybridResultRow, meta hybrid.ResultMeta, explain bool) error {
	rows := make([]map[string]any, len(results))
	for i, r := range results {
		row := map[string]any{
			"id":         r.MessageID,
			"subject":    r.Subject,
			"from_email": r.FromEmail,
			"sent_at":    r.SentAt.Format(time.RFC3339),
			"boosted":    r.SubjectBoosted,
		}
		// rrf_score is omitted when NaN (the convention pure vector
		// mode uses to mark "no fusion happened"). encoding/json
		// rejects NaN, so emitting it would also fail the encode.
		if !math.IsNaN(r.RRFScore) {
			row["rrf_score"] = r.RRFScore
		}
		if explain {
			if !math.IsNaN(r.BM25Score) {
				row["bm25_score"] = r.BM25Score
			}
			if !math.IsNaN(r.VectorScore) {
				row["vector_score"] = r.VectorScore
			}
		}
		rows[i] = row
	}
	return printJSON(map[string]any{
		"generation": map[string]any{
			"id":          int64(meta.Generation.ID),
			"model":       meta.Generation.Model,
			"dimension":   meta.Generation.Dimension,
			"fingerprint": meta.Generation.Fingerprint,
			"state":       string(meta.Generation.State),
		},
		"pool_saturated": meta.PoolSaturated,
		"returned_count": meta.ReturnedCount,
		"results":        rows,
	})
}

func formatOptionalScore(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.4f", v)
}
