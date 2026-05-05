package cmd

import (
	"compress/zlib"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/mime"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/textutil"
)

var repairEncodingCmd = &cobra.Command{
	Use:   "repair-encoding",
	Short: "Repair invalid UTF-8 encoding in message text fields",
	Long: `Scan for messages with invalid UTF-8 and repair them.

This command repairs invalid UTF-8 in:
- Subject
- Body text
- Body HTML
- Snippet
- Participant display names, email addresses, and domains
- Conversation titles and source IDs
- Label names and attachment filenames

For each invalid field, it:
1. Re-parses the raw MIME data to extract text with proper charset handling
2. If re-parsing fails, attempts charset detection (Windows-1252, Latin-1, etc.)
3. As a last resort, replaces invalid bytes with the replacement character

This is useful after a sync that may have produced invalid UTF-8 due to
charset detection issues in the MIME parser.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		dbPath := cfg.DatabaseDSN()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = s.Close() }()

		if err := s.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrations(s); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		reembedNeededIDs, err := repairEncoding(s)
		if err != nil {
			return err
		}

		// Re-enqueue repaired messages for re-embedding so semantic
		// results reflect the corrected text. setupVectorFeatures
		// returns (nil, nil) when vector search is disabled or the
		// binary was built without sqlite_vec; in those cases the
		// pending_embeddings table is irrelevant and there's nothing
		// to do.
		if len(reembedNeededIDs) > 0 {
			vf, err := setupVectorFeatures(ctx, s.DB(), dbPath)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"Warning: failed to open vectors.db for re-enqueue: %v\n", err)
			} else if vf != nil {
				if err := vf.Enqueuer.EnqueueMessages(ctx, reembedNeededIDs); err != nil {
					fmt.Fprintf(os.Stderr,
						"Warning: failed to re-enqueue %d messages for re-embedding: %v\n",
						len(reembedNeededIDs), err)
				} else {
					fmt.Printf("Re-enqueued %d message(s) for re-embedding.\n",
						len(reembedNeededIDs))
				}
				if closeErr := vf.Close(); closeErr != nil {
					logger.Warn("closing vectors.db failed", "error", closeErr)
				}
			}
		}

		analyticsDir := cfg.AnalyticsDir()
		if _, err := buildCache(dbPath, analyticsDir, true); err != nil {
			fmt.Fprintf(os.Stderr,
				"Warning: cache rebuild failed: %v\n", err)
			fmt.Fprintf(os.Stderr,
				"Run 'msgvault build-cache --full-rebuild' to retry.\n")
		} else {
			fmt.Println("\nAnalytics cache rebuilt.")
		}
		return nil
	},
}

// repairStats tracks repair statistics
type repairStats struct {
	subjects      int
	bodyTexts     int
	bodyHTMLs     int
	snippets      int
	displayNames  int
	labels        int
	filenames     int
	convTitles    int
	convSourceIDs int
	emailAddrs    int
	domains       int
	skippedRows   int
}

// repairEncoding runs all repair passes over s and returns the IDs of
// messages whose embedding inputs (subject, body_text, or body_html)
// were modified. Callers use that list to re-enqueue affected messages
// for re-embedding so semantic search results don't stay stale against
// the repaired text. Snippet-only repairs are NOT included because the
// embedder doesn't read snippet.
func repairEncoding(s *store.Store) (reembedNeededIDs []int64, err error) {
	stats := &repairStats{}

	// Repair message text fields
	reembedNeededIDs, err = repairMessageFields(s, stats)
	if err != nil {
		return nil, err
	}

	// Repair display names in participants and message_recipients
	if err := repairDisplayNames(s, stats); err != nil {
		return nil, err
	}

	// Repair other string fields that could have encoding issues
	if err := repairOtherStrings(s, stats); err != nil {
		return nil, err
	}

	// Summary
	total := stats.subjects + stats.bodyTexts + stats.bodyHTMLs + stats.snippets +
		stats.displayNames + stats.labels + stats.filenames + stats.convTitles +
		stats.convSourceIDs + stats.emailAddrs + stats.domains
	if total == 0 {
		fmt.Println("No encoding repairs needed.")
		return nil, nil
	}

	fmt.Println("\n=== Repair Summary ===")
	if stats.subjects > 0 {
		fmt.Printf("  Subjects:      %d\n", stats.subjects)
	}
	if stats.bodyTexts > 0 {
		fmt.Printf("  Body texts:    %d\n", stats.bodyTexts)
	}
	if stats.bodyHTMLs > 0 {
		fmt.Printf("  Body HTMLs:    %d\n", stats.bodyHTMLs)
	}
	if stats.snippets > 0 {
		fmt.Printf("  Snippets:      %d\n", stats.snippets)
	}
	if stats.displayNames > 0 {
		fmt.Printf("  Display names: %d\n", stats.displayNames)
	}
	if stats.labels > 0 {
		fmt.Printf("  Labels:        %d\n", stats.labels)
	}
	if stats.filenames > 0 {
		fmt.Printf("  Filenames:     %d\n", stats.filenames)
	}
	if stats.convTitles > 0 {
		fmt.Printf("  Conv titles:   %d\n", stats.convTitles)
	}
	if stats.convSourceIDs > 0 {
		fmt.Printf("  Conv src IDs:  %d\n", stats.convSourceIDs)
	}
	if stats.emailAddrs > 0 {
		fmt.Printf("  Email addrs:   %d\n", stats.emailAddrs)
	}
	if stats.domains > 0 {
		fmt.Printf("  Domains:       %d\n", stats.domains)
	}
	if stats.skippedRows > 0 {
		fmt.Printf("  Skipped rows:  %d (scan errors)\n", stats.skippedRows)
	}
	fmt.Printf("  Total fields:  %d\n", total)
	return reembedNeededIDs, nil
}

func repairMessageFields(s *store.Store, stats *repairStats) (reembedNeededIDs []int64, err error) {
	fmt.Println("Scanning messages for invalid UTF-8...")

	db := s.DB()

	// Query all messages with their raw data
	rows, err := db.Query(`
		SELECT m.id, m.subject, mb.body_text, mb.body_html, m.snippet,
		       mr.raw_data, mr.compression
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		LEFT JOIN message_raw mr ON mr.message_id = m.id
	`)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type messageRepair struct {
		id                                       int64
		newSubject, newBody, newHTML, newSnippet sql.NullString
	}

	const batchSize = 1000
	var repairs []messageRepair
	scanned := 0
	totalRepaired := 0

	// Helper to apply a batch of repairs
	applyBatch := func() error {
		if len(repairs) == 0 {
			return nil
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}

		for _, r := range repairs {
			// Update messages table (subject, snippet)
			var msgUpdates []string
			var msgArgs []interface{}
			if r.newSubject.Valid {
				msgUpdates = append(msgUpdates, "subject = ?")
				msgArgs = append(msgArgs, r.newSubject.String)
			}
			if r.newSnippet.Valid {
				msgUpdates = append(msgUpdates, "snippet = ?")
				msgArgs = append(msgArgs, r.newSnippet.String)
			}
			if len(msgUpdates) > 0 {
				msgArgs = append(msgArgs, r.id)
				query := s.Rebind(fmt.Sprintf("UPDATE messages SET %s WHERE id = ?", strings.Join(msgUpdates, ", ")))
				if _, err := tx.Exec(query, msgArgs...); err != nil {
					if rbErr := tx.Rollback(); rbErr != nil {
						logger.Warn("rollback failed", "error", rbErr)
					}
					return fmt.Errorf("update message %d: %w", r.id, err)
				}
			}

			// Upsert message_bodies table (body_text, body_html)
			// Use INSERT ON CONFLICT to handle rows that may not exist yet
			if r.newBody.Valid || r.newHTML.Valid {
				var bodyText, bodyHTML interface{}
				if r.newBody.Valid {
					bodyText = r.newBody.String
				}
				if r.newHTML.Valid {
					bodyHTML = r.newHTML.String
				}
				query := s.Rebind(`INSERT INTO message_bodies (message_id, body_text, body_html)
					VALUES (?, ?, ?)
					ON CONFLICT(message_id) DO UPDATE SET
						body_text = COALESCE(excluded.body_text, message_bodies.body_text),
						body_html = COALESCE(excluded.body_html, message_bodies.body_html)`)
				if _, err := tx.Exec(query, r.id, bodyText, bodyHTML); err != nil {
					if rbErr := tx.Rollback(); rbErr != nil {
						logger.Warn("rollback failed", "error", rbErr)
					}
					return fmt.Errorf("upsert message_bodies %d: %w", r.id, err)
				}
			}

			// Any change to fields that feed the embedder (subject,
			// body_text, body_html) invalidates prior embeddings and
			// must trigger a re-enqueue. Snippet is not embedded, so
			// snippet-only repairs are excluded.
			if r.newSubject.Valid || r.newBody.Valid || r.newHTML.Valid {
				reembedNeededIDs = append(reembedNeededIDs, r.id)
			}
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		totalRepaired += len(repairs)
		repairs = repairs[:0] // Clear slice, keep capacity
		return nil
	}

	for rows.Next() {
		var id int64
		var subject, bodyText, bodyHTML, snippet sql.NullString
		var rawData []byte
		var compression sql.NullString

		if err := rows.Scan(&id, &subject, &bodyText, &bodyHTML, &snippet, &rawData, &compression); err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping message row: scan error: %v\n", err)
			stats.skippedRows++
			continue
		}

		scanned++
		if scanned%100000 == 0 {
			fmt.Printf("Scanned %d messages...\n", scanned)
		}

		// Check each field for invalid UTF-8
		var repair messageRepair
		repair.id = id
		var parsed *mime.Message
		needsRepair := false

		// Subject
		if subject.Valid && !utf8.ValidString(subject.String) {
			if parsed == nil {
				parsed = tryParseMIME(rawData, compression)
			}
			if parsed != nil && utf8.ValidString(parsed.Subject) {
				repair.newSubject = sql.NullString{String: parsed.Subject, Valid: true}
			} else {
				repair.newSubject = sql.NullString{String: textutil.EnsureUTF8(subject.String), Valid: true}
			}
			needsRepair = true
			stats.subjects++
		}

		// Body text
		if bodyText.Valid && !utf8.ValidString(bodyText.String) {
			if parsed == nil {
				parsed = tryParseMIME(rawData, compression)
			}
			if parsed != nil && utf8.ValidString(parsed.GetBodyText()) {
				repair.newBody = sql.NullString{String: parsed.GetBodyText(), Valid: true}
			} else {
				repair.newBody = sql.NullString{String: textutil.EnsureUTF8(bodyText.String), Valid: true}
			}
			needsRepair = true
			stats.bodyTexts++
		}

		// Body HTML
		if bodyHTML.Valid && !utf8.ValidString(bodyHTML.String) {
			if parsed == nil {
				parsed = tryParseMIME(rawData, compression)
			}
			if parsed != nil && utf8.ValidString(parsed.BodyHTML) {
				repair.newHTML = sql.NullString{String: parsed.BodyHTML, Valid: true}
			} else {
				repair.newHTML = sql.NullString{String: textutil.EnsureUTF8(bodyHTML.String), Valid: true}
			}
			needsRepair = true
			stats.bodyHTMLs++
		}

		// Snippet (from Gmail API, not in raw MIME)
		if snippet.Valid && !utf8.ValidString(snippet.String) {
			repair.newSnippet = sql.NullString{String: textutil.EnsureUTF8(snippet.String), Valid: true}
			needsRepair = true
			stats.snippets++
		}

		if needsRepair {
			repairs = append(repairs, repair)

			// Apply batch when full
			if len(repairs) >= batchSize {
				if err := applyBatch(); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	// Apply remaining repairs
	if err := applyBatch(); err != nil {
		return nil, err
	}

	if totalRepaired > 0 {
		fmt.Printf("Repaired %d messages\n", totalRepaired)
	} else {
		fmt.Println("No messages needed repair")
	}
	return reembedNeededIDs, nil
}

func repairDisplayNames(s *store.Store, stats *repairStats) error {
	db := s.DB()

	// Repair display names in both message_recipients and participants tables
	tables := []struct {
		name       string
		query      string
		updateStmt string
	}{
		{
			name:       "message_recipients",
			query:      "SELECT id, display_name FROM message_recipients WHERE display_name IS NOT NULL",
			updateStmt: "UPDATE message_recipients SET display_name = ? WHERE id = ?",
		},
		{
			name:       "participants",
			query:      "SELECT id, display_name FROM participants WHERE display_name IS NOT NULL",
			updateStmt: "UPDATE participants SET display_name = ? WHERE id = ?",
		},
	}

	for _, table := range tables {
		fmt.Printf("Scanning %s display names for invalid UTF-8...\n", table.name)

		rows, err := db.Query(table.query)
		if err != nil {
			return fmt.Errorf("query %s: %w", table.name, err)
		}

		type nameRepair struct {
			id      int64
			newName string
		}

		const batchSize = 1000
		var repairs []nameRepair
		scanned := 0
		totalRepaired := 0

		// Helper to apply a batch of repairs
		applyBatch := func() error {
			if len(repairs) == 0 {
				return nil
			}

			tx, err := db.Begin()
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}

			stmt, err := tx.Prepare(s.Rebind(table.updateStmt))
			if err != nil {
				if rbErr := tx.Rollback(); rbErr != nil {
					logger.Warn("rollback failed", "error", rbErr)
				}
				return fmt.Errorf("prepare update: %w", err)
			}
			defer func() { _ = stmt.Close() }()

			for _, r := range repairs {
				if _, err := stmt.Exec(r.newName, r.id); err != nil {
					if rbErr := tx.Rollback(); rbErr != nil {
						logger.Warn("rollback failed", "error", rbErr)
					}
					return fmt.Errorf("update %s %d: %w", table.name, r.id, err)
				}
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			totalRepaired += len(repairs)
			repairs = repairs[:0] // Clear slice, keep capacity
			return nil
		}

		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				fmt.Fprintf(os.Stderr, "warning: skipping %s row: scan error: %v\n", table.name, err)
				stats.skippedRows++
				continue
			}

			scanned++
			if scanned%100000 == 0 {
				fmt.Printf("Scanned %d %s display names...\n", scanned, table.name)
			}

			if !utf8.ValidString(name) {
				repairs = append(repairs, nameRepair{id: id, newName: textutil.EnsureUTF8(name)})
				stats.displayNames++

				// Apply batch when full
				if len(repairs) >= batchSize {
					if err := applyBatch(); err != nil {
						_ = rows.Close()
						return err
					}
				}
			}
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate %s: %w", table.name, err)
		}
		_ = rows.Close()

		// Apply remaining repairs
		if err := applyBatch(); err != nil {
			return err
		}

		if totalRepaired > 0 {
			fmt.Printf("Repaired %d %s display names\n", totalRepaired, table.name)
		}
	}

	return nil
}

// repairOtherStrings repairs other string fields that could have encoding issues
func repairOtherStrings(s *store.Store, stats *repairStats) error {
	db := s.DB()

	// Tables and columns to repair
	tables := []struct {
		name       string
		column     string
		query      string
		updateStmt string
		counter    *int
	}{
		{
			name:       "labels",
			column:     "name",
			query:      "SELECT id, name FROM labels WHERE name IS NOT NULL",
			updateStmt: "UPDATE labels SET name = ? WHERE id = ?",
			counter:    &stats.labels,
		},
		{
			name:       "attachments",
			column:     "filename",
			query:      "SELECT id, filename FROM attachments WHERE filename IS NOT NULL",
			updateStmt: "UPDATE attachments SET filename = ? WHERE id = ?",
			counter:    &stats.filenames,
		},
		{
			name:       "conversations",
			column:     "title",
			query:      "SELECT id, title FROM conversations WHERE title IS NOT NULL",
			updateStmt: "UPDATE conversations SET title = ? WHERE id = ?",
			counter:    &stats.convTitles,
		},
		{
			name:       "conversations",
			column:     "source_conversation_id",
			query:      "SELECT id, source_conversation_id FROM conversations WHERE source_conversation_id IS NOT NULL",
			updateStmt: "UPDATE conversations SET source_conversation_id = ? WHERE id = ?",
			counter:    &stats.convSourceIDs,
		},
		{
			name:       "participants",
			column:     "email_address",
			query:      "SELECT id, email_address FROM participants WHERE email_address IS NOT NULL",
			updateStmt: "UPDATE participants SET email_address = ? WHERE id = ?",
			counter:    &stats.emailAddrs,
		},
		{
			name:       "participants",
			column:     "domain",
			query:      "SELECT id, domain FROM participants WHERE domain IS NOT NULL",
			updateStmt: "UPDATE participants SET domain = ? WHERE id = ?",
			counter:    &stats.domains,
		},
	}

	for _, table := range tables {
		fmt.Printf("Scanning %s.%s for invalid UTF-8...\n", table.name, table.column)

		rows, err := db.Query(table.query)
		if err != nil {
			return fmt.Errorf("query %s: %w", table.name, err)
		}

		type repair struct {
			id       int64
			newValue string
		}

		const batchSize = 1000
		var repairs []repair
		scanned := 0
		totalRepaired := 0

		applyBatch := func() error {
			if len(repairs) == 0 {
				return nil
			}

			tx, err := db.Begin()
			if err != nil {
				return fmt.Errorf("begin transaction: %w", err)
			}

			stmt, err := tx.Prepare(s.Rebind(table.updateStmt))
			if err != nil {
				if rbErr := tx.Rollback(); rbErr != nil {
					logger.Warn("rollback failed", "error", rbErr)
				}
				return fmt.Errorf("prepare update: %w", err)
			}
			defer func() { _ = stmt.Close() }()

			for _, r := range repairs {
				if _, err := stmt.Exec(r.newValue, r.id); err != nil {
					if rbErr := tx.Rollback(); rbErr != nil {
						logger.Warn("rollback failed", "error", rbErr)
					}
					return fmt.Errorf("update %s %d: %w", table.name, r.id, err)
				}
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			totalRepaired += len(repairs)
			repairs = repairs[:0]
			return nil
		}

		for rows.Next() {
			var id int64
			var value string
			if err := rows.Scan(&id, &value); err != nil {
				fmt.Fprintf(os.Stderr, "warning: skipping %s.%s row: scan error: %v\n", table.name, table.column, err)
				stats.skippedRows++
				continue
			}

			scanned++
			if scanned%100000 == 0 {
				fmt.Printf("Scanned %d %s.%s...\n", scanned, table.name, table.column)
			}

			if !utf8.ValidString(value) {
				repairs = append(repairs, repair{id: id, newValue: textutil.EnsureUTF8(value)})
				*table.counter++

				if len(repairs) >= batchSize {
					if err := applyBatch(); err != nil {
						_ = rows.Close()
						return err
					}
				}
			}
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate %s: %w", table.name, err)
		}
		_ = rows.Close()

		if err := applyBatch(); err != nil {
			return err
		}

		if totalRepaired > 0 {
			fmt.Printf("Repaired %d %s.%s values\n", totalRepaired, table.name, table.column)
		}
	}

	return nil
}

// tryParseMIME attempts to parse raw MIME data, returning nil on failure
func tryParseMIME(rawData []byte, compression sql.NullString) *mime.Message {
	if len(rawData) == 0 {
		return nil
	}

	// Decompress if needed
	if compression.Valid && compression.String == "zlib" {
		r, err := zlib.NewReader(io.NopCloser(&byteReader{data: rawData}))
		if err != nil {
			return nil
		}
		rawData, err = io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			return nil
		}
	}

	parsed, err := mime.Parse(rawData)
	if err != nil {
		return nil
	}
	return parsed
}

// byteReader wraps a byte slice for use with zlib.NewReader
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func init() {
	rootCmd.AddCommand(repairEncodingCmd)
}
