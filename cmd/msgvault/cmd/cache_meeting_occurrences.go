package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/store"
)

func stageMeetingOccurrences(ctx context.Context, db sqlRunner, evidence *store.MeetingEvidence, root, committed string) (time.Time, error) {
	parquet := func(dataset string) string {
		dir := filepath.Join(root, dataset)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			dir = filepath.Join(committed, dataset)
		}
		return "read_parquet('" + quoteCacheSQL(filepath.Join(dir, "*.parquet")) + "')"
	}
	participants, identifiers, clusters, owners := parquet(tableParticipants), parquet(tableParticipantIdentifiers), parquet(tableParticipantClusters), parquet(tableOwnerParticipants)
	query := `WITH canon AS (` + identityindex.CanonicalParticipantsSQL(participants, clusters) + `),
owner_canon AS (` + identityindex.OwnerCanonicalsSQL(owners, "canon") + `),
emails AS (SELECT id AS participant_id, email_address AS email FROM ` + participants + ` UNION ALL SELECT participant_id, identifier_value AS email FROM ` + identifiers + ` WHERE identifier_type='email')
SELECT DISTINCT lower(trim(e.email)) FROM emails e JOIN canon c ON c.participant_id=e.participant_id JOIN owner_canon o ON o.canonical_id=c.canonical_id WHERE e.email IS NOT NULL AND trim(e.email) <> ''`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return time.Time{}, fmt.Errorf("read meeting owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ownerEmails := map[string]bool{}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			_ = rows.Close()
			return time.Time{}, err
		}
		ownerEmails[email] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return time.Time{}, err
	}
	if err := rows.Close(); err != nil {
		return time.Time{}, err
	}
	occurrences, nextAt, err := evidence.Reduce(ctx, ownerEmails)
	if err != nil {
		return time.Time{}, fmt.Errorf("read meeting occurrences: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE tmp_meeting_occurrences (message_id BIGINT, anchor_message_id BIGINT, occurrence_key VARCHAR, occurred_at TIMESTAMP, start_date DATE, counted BOOLEAN, declined_emails VARCHAR[])`); err != nil {
		return time.Time{}, fmt.Errorf("create meeting occurrence table: %w", err)
	}
	defer func() { _, _ = db.ExecContext(context.WithoutCancel(ctx), `DROP TABLE tmp_meeting_occurrences`) }()
	var values []string
	var args []any
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tmp_meeting_occurrences VALUES `+strings.Join(values, ","), args...); err != nil {
			return fmt.Errorf("stage meeting occurrences: %w", err)
		}
		values, args = values[:0], args[:0]
		return nil
	}
	for _, occurrence := range occurrences {
		values = append(values, `(?, ?, ?, ?, NULLIF(?, '')::DATE, ?, [`+strings.TrimSuffix(strings.Repeat("?,", len(occurrence.DeclinedEmails)), ",")+`]::VARCHAR[])`)
		var occurredAt any
		if !occurrence.OccurredAt.IsZero() {
			occurredAt = occurrence.OccurredAt.UTC().Format("2006-01-02 15:04:05.999999999")
		}
		args = append(args, occurrence.MessageID, occurrence.AnchorMessageID, occurrence.OccurrenceKey, occurredAt, occurrence.StartDate, occurrence.Counted)
		for _, email := range occurrence.DeclinedEmails {
			args = append(args, email)
		}
		if len(values) >= participantClusterMaxBatchRows {
			if err := flush(); err != nil {
				return time.Time{}, err
			}
		}
	}
	if err := flush(); err != nil {
		return time.Time{}, err
	}
	err = copyParquet(ctx, db, filepath.Join(root, identityindex.DatasetMeetingOccurrences), "meeting_occurrences.parquet", `SELECT *, CAST(-9223372036854775808::HUGEINT - dense_rank() OVER (ORDER BY occurrence_key) AS DECIMAL(38,0)) AS occurrence_id FROM tmp_meeting_occurrences`)
	return nextAt, err
}
