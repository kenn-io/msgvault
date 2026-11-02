package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"go.kenn.io/msgvault/internal/mailrecords"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/verification"
)

const tableMessageRecords = "message_records"

// Records are a replaceable projection of the pinned source snapshot. Body
// hydration happens by primary key before Prepare closes the CSV transaction;
// no body table is joined into a metadata list or analytics query.
func stageMailRecords(ctx context.Context, snapshot *cacheSourceSnapshot, through int64) error {
	db := snapshot.DuckDB()
	if _, err := db.ExecContext(ctx, "CREATE TEMP TABLE tmp_message_records AS "+query.EmptyMessageRecordsSQL); err != nil {
		return fmt.Errorf("create message records projection: %w", err)
	}
	var exists int
	if err := snapshot.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='message_bodies'`).Scan(&exists); err != nil {
		return fmt.Errorf("inspect message bodies: %w", err)
	}
	if exists == 0 {
		return nil
	}
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Materialize only a small ID batch, closing its reader before any
		// primary-key hydration or DuckDB writes on the scanner connection.
		var encoded string
		if err := snapshot.QueryRow(`SELECT COALESCE(json_group_array(id), '[]') FROM
			(SELECT id FROM messages WHERE id > ? AND id <= ? AND `+exportableMessageWhere("")+` ORDER BY id LIMIT 128)`, after, through).Scan(&encoded); err != nil {
			return fmt.Errorf("read message records candidates: %w", err)
		}
		var ids []int64
		if err := json.Unmarshal([]byte(encoded), &ids); err != nil {
			return fmt.Errorf("decode message records candidates: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			after = id
			if err := stageMessageRecords(ctx, snapshot, id); err != nil {
				return err
			}
		}
	}
}

func stageMessageRecords(ctx context.Context, snapshot *cacheSourceSnapshot, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var text, body string
	err := snapshot.QueryRow(`SELECT COALESCE(body_text,''), COALESCE(body_html,'')
		FROM message_bodies WHERE message_id = ?
		AND length(CAST(COALESCE(body_html,'') AS BLOB)) <= ?
		AND length(CAST(COALESCE(body_text,'') AS BLOB)) <= ?
		AND instr(lower(substr(body_html,1,?)), 'application/ld+json') > 0`,
		id, mailrecords.MaxHTMLBytes, mailrecords.MaxHTMLBytes, mailrecords.MaxHTMLBytes).Scan(&text, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hydrate message records body %d: %w", id, err)
	}
	var subject, snippet, messageType string
	if err := snapshot.QueryRow(`SELECT COALESCE(subject,''), COALESCE(snippet,''), COALESCE(message_type,'') FROM messages WHERE id = ?`, id).Scan(&subject, &snippet, &messageType); err != nil {
		return fmt.Errorf("read message records metadata %d: %w", id, err)
	}
	if verification.Detect(verification.Input{MessageType: messageType, Subject: subject, Snippet: snippet, BodyText: text, BodyHTML: body}) != nil {
		return nil
	}
	result := mailrecords.Extract(mailrecords.Input{Text: text, HTML: body})
	for _, record := range result.Records {
		if err := insertMessageRecord(ctx, snapshot.DuckDB(), id, record); err != nil {
			return err
		}
	}
	return nil
}

func insertMessageRecord(ctx context.Context, db sqlRunner, id int64, record mailrecords.Record) error {
	kinds := map[string]string{"Order": "order", "ParcelDelivery": "shipment", "Invoice": "invoice", "FlightReservation": "flight", "LodgingReservation": "lodging", "EventReservation": "event_ticket"}
	// Retain the trust wrappers and rare fields for inspection. Reservation
	// confirmation references are not credentials; verification messages were
	// excluded above by the shared detector before any column was written.
	extra, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode message record: %w", err)
	}
	names := []string{"merchant", "order_number", "amount", "currency", "tracking_number", "carrier", "status", "starts_at", "ends_at", "origin", "destination", "confirmation_code"}
	args := []any{id, kinds[record.Type], "jsonld"}
	for _, name := range names {
		value := record.Fields[name].Value
		if number, ok := value.(json.Number); ok {
			value = number.String()
		}
		args = append(args, value)
	}
	args = append(args, record.Dropped, mailrecords.ExtractorVersion, string(extra))
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	if _, err := db.ExecContext(ctx, "INSERT INTO tmp_message_records VALUES ("+placeholders+")", args...); err != nil {
		return fmt.Errorf("stage message record %d: %w", id, err)
	}
	return nil
}

func exportMailRecords(ctx context.Context, snapshot *cacheSourceSnapshot, root string) error {
	return copyParquet(ctx, snapshot.DuckDB(), filepath.Join(root, tableMessageRecords), "records.parquet", "SELECT * FROM tmp_message_records")
}
