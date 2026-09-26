package muesli

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" database/sql driver
)

// maxFolderDepth bounds folder-path resolution so a corrupt parent cycle
// cannot loop forever.
const maxFolderDepth = 32

// Reader reads meetings from a Muesli database without changing it. Muesli
// runs its schema migration and tombstone purge whenever it (or muesli-cli)
// opens the database, so msgvault reads the file directly instead.
type Reader struct {
	db      *sql.DB
	path    string
	columns map[string]map[string]bool
}

// Open opens the Muesli database at path and checks that it looks like a
// Muesli database. SQLite may use the WAL sidecar files to coordinate with a
// running Muesli app; query_only prevents this connection from changing data.
func Open(ctx context.Context, path string) (*Reader, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("open Muesli database %s: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("open Muesli database %s: %w", abs, err)
	}
	db, err := openQueryOnly(abs)
	if err != nil {
		return nil, fmt.Errorf("open Muesli database %s: %w", abs, err)
	}
	reader := &Reader{db: db, path: abs}
	if err := reader.inspect(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return reader, nil
}

// openQueryOnly opens another app's live SQLite store without changing it.
// url.URL escapes '#', '?', and '%' in the path so they cannot end the
// filename early. query_only rejects write SQL while still letting SQLite
// manage WAL sidecars. immutable=1 is deliberately absent: it would ignore
// rows the owning app has committed to the WAL.
func openQueryOnly(abs string) (*sql.DB, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     sqliteURIPath(abs),
		RawQuery: "_busy_timeout=5000&_query_only=1",
	}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// sqliteURIPath returns an absolute path in the form expected in a file URI.
// Windows drive letters need a leading slash so url.URL produces file:///C:/…
// instead of treating C: as the URI authority.
func sqliteURIPath(path string) string {
	slashed := filepath.ToSlash(path)
	volume := filepath.VolumeName(path)
	if len(volume) == 2 && volume[1] == ':' {
		return "/" + slashed
	}
	return slashed
}

// Close releases the database handle.
func (r *Reader) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *Reader) inspect(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		return fmt.Errorf("open Muesli database %s: %w", r.path, err)
	}
	r.columns = map[string]map[string]bool{}
	for _, table := range []string{"meetings", "meeting_participants", "meeting_folders", "meeting_participant_suppressions"} {
		columns, err := tableColumns(ctx, r.db, table)
		if err != nil {
			return fmt.Errorf("inspect Muesli database %s: %w", r.path, err)
		}
		r.columns[table] = columns
	}
	for _, required := range []string{"id", "title", "start_time"} {
		if !r.columns["meetings"][required] {
			return fmt.Errorf("%s is not a Muesli database: meetings.%s is missing", r.path, required)
		}
	}
	return nil
}

// tableColumns returns the column names of table, or an empty set when the
// table does not exist.
func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (r *Reader) has(table, column string) bool {
	return r.columns[table][column]
}

// column selects alias.column when the table has it and NULL otherwise, so
// the same query works across Muesli schema generations.
func (r *Reader) column(table, alias, column string) string {
	if r.has(table, column) {
		return alias + "." + column
	}
	return "NULL"
}

// ListMeetings returns every meeting in ascending id order, including
// deleted and in-progress rows; callers decide what to archive. All reads
// share one transaction so they see a single WAL snapshot.
func (r *Reader) ListMeetings(ctx context.Context) ([]Meeting, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read Muesli meetings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	folders, err := r.folderPaths(ctx, tx)
	if err != nil {
		return nil, err
	}
	meetings, err := r.meetings(ctx, tx, folders)
	if err != nil {
		return nil, err
	}
	participants, err := r.participants(ctx, tx)
	if err != nil {
		return nil, err
	}
	for i := range meetings {
		meetings[i].Participants = participants[meetings[i].ID]
	}
	return meetings, nil
}

func (r *Reader) meetings(ctx context.Context, tx *sql.Tx, folders map[int64]string) ([]Meeting, error) {
	col := func(name string) string { return r.column("meetings", "m", name) }
	query := `SELECT m.id, m.title, m.start_time, ` +
		col("end_time") + `, ` +
		col("created_at") + `, ` +
		col("duration_seconds") + `, ` +
		col("meeting_status") + `, ` +
		col("source") + `, ` +
		col("raw_transcript") + `, ` +
		col("formatted_notes") + `, ` +
		col("manual_notes") + `, ` +
		col("word_count") + `, ` +
		col("selected_template_name") + `, ` +
		col("selected_template_kind") + `, ` +
		col("calendar_event_id") + `, ` +
		col("calendar_source") + `, ` +
		col("calendar_series_id") + `, ` +
		col("folder_id") + `, ` +
		col("follow_up_to_id") + `, ` +
		col("deleted_at") + ` IS NOT NULL
		FROM meetings m ORDER BY m.id`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read Muesli meetings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var meetings []Meeting
	for rows.Next() {
		var (
			m                                               Meeting
			title, start                                    sql.NullString
			end, created, status, source, transcript, notes sql.NullString
			manual, templateName, templateKind, eventID     sql.NullString
			calendarSource, seriesID                        sql.NullString
			duration                                        sql.NullFloat64
			wordCount, folderID, followUpID                 sql.NullInt64
		)
		if err := rows.Scan(&m.ID, &title, &start, &end, &created, &duration, &status, &source,
			&transcript, &notes, &manual, &wordCount, &templateName, &templateKind,
			&eventID, &calendarSource, &seriesID, &folderID, &followUpID, &m.Deleted); err != nil {
			return nil, fmt.Errorf("read Muesli meeting row: %w", err)
		}
		m.Title, m.StartTime, m.EndTime, m.CreatedAt = title.String, start.String, end.String, created.String
		m.DurationSeconds, m.Status, m.Source = duration.Float64, status.String, source.String
		m.RawTranscript, m.FormattedNotes, m.ManualNotes = transcript.String, notes.String, manual.String
		m.WordCount, m.TemplateName, m.TemplateKind = wordCount.Int64, templateName.String, templateKind.String
		m.CalendarEventID, m.CalendarSource, m.CalendarSeriesID = eventID.String, calendarSource.String, seriesID.String
		m.FollowUpToID = followUpID.Int64
		if folderID.Valid {
			m.Folder = folders[folderID.Int64]
		}
		meetings = append(meetings, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Muesli meetings: %w", err)
	}
	return meetings, nil
}

// participants returns non-suppressed participants keyed by meeting id, in
// Muesli's display order. Older databases kept suppressions in a separate
// table that Muesli later folded into is_suppressed.
func (r *Reader) participants(ctx context.Context, tx *sql.Tx) (map[int64][]Participant, error) {
	out := map[int64][]Participant{}
	if !r.has("meeting_participants", "meeting_id") {
		return out, nil
	}
	suppressed := "1 = 1"
	switch {
	case r.has("meeting_participants", "is_suppressed"):
		suppressed = "COALESCE(p.is_suppressed, 0) = 0"
	case r.has("meeting_participant_suppressions", "participant_identifier"):
		suppressed = `NOT EXISTS (
			SELECT 1 FROM meeting_participant_suppressions s
			WHERE s.meeting_id = p.meeting_id
			  AND s.participant_identifier = p.participant_identifier)`
	}
	query := `SELECT p.meeting_id, p.participant_identifier, p.display_name, ` +
		r.column("meeting_participants", "p", "email_address") + `, ` +
		r.column("meeting_participants", "p", "source") + `
		FROM meeting_participants p
		WHERE ` + suppressed + `
		ORDER BY p.meeting_id, p.insertion_order, p.participant_identifier`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read Muesli participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			meetingID                   int64
			identifier, name            sql.NullString
			emailAddress, participantOf sql.NullString
		)
		if err := rows.Scan(&meetingID, &identifier, &name, &emailAddress, &participantOf); err != nil {
			return nil, fmt.Errorf("read Muesli participant row: %w", err)
		}
		email := strings.ToLower(strings.TrimSpace(emailAddress.String))
		if email == "" {
			if rest, ok := strings.CutPrefix(identifier.String, "email:"); ok {
				email = strings.ToLower(strings.TrimSpace(rest))
			}
		}
		out[meetingID] = append(out[meetingID], Participant{
			Name:       strings.TrimSpace(name.String),
			Email:      email,
			Source:     strings.TrimSpace(participantOf.String),
			Identifier: strings.TrimSpace(identifier.String),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Muesli participants: %w", err)
	}
	return out, nil
}

// folderPaths resolves every folder to its "Parent/Child" path.
func (r *Reader) folderPaths(ctx context.Context, tx *sql.Tx) (map[int64]string, error) {
	paths := map[int64]string{}
	if !r.has("meeting_folders", "id") || !r.has("meeting_folders", "name") {
		return paths, nil
	}
	query := `SELECT f.id, f.name, ` + r.column("meeting_folders", "f", "parent_id") +
		` FROM meeting_folders f`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read Muesli folders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type folder struct {
		name   string
		parent sql.NullInt64
	}
	folders := map[int64]folder{}
	for rows.Next() {
		var (
			id     int64
			name   sql.NullString
			parent sql.NullInt64
		)
		if err := rows.Scan(&id, &name, &parent); err != nil {
			return nil, fmt.Errorf("read Muesli folder row: %w", err)
		}
		folders[id] = folder{name: strings.TrimSpace(name.String), parent: parent}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Muesli folders: %w", err)
	}
	for id := range folders {
		var parts []string
		current, ok := folders[id], true
		for depth := 0; ok && depth < maxFolderDepth; depth++ {
			parts = append([]string{current.name}, parts...)
			if !current.parent.Valid {
				break
			}
			current, ok = folders[current.parent.Int64]
		}
		paths[id] = strings.Join(parts, "/")
	}
	return paths, nil
}
