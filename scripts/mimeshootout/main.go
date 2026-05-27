// MIME Library Shootout
//
// Compares enmime vs go-message parsing against Python's email.parser output.
// Reads raw MIME from message_raw table (zlib compressed) and compares
// extracted fields against what Python stored in messages/participants tables.
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jhillyerd/enmime"
	_ "github.com/mattn/go-sqlite3"
)

type PythonMessage struct {
	ID          int64
	SourceMsgID string
	Subject     sql.NullString
	BodyText    sql.NullString
	SentAt      sql.NullString

	// Participants extracted separately
	FromAddresses []Address
	ToAddresses   []Address
	CcAddresses   []Address
	BccAddresses  []Address

	// Attachment count
	AttachmentCount int
}

type Address struct {
	Name  string
	Email string
}

type ParseResult struct {
	Subject     string
	BodyText    string
	From        []Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	Attachments int
	Error       error
}

type MismatchType string

const (
	MismatchSubject      MismatchType = "subject"
	MismatchFrom         MismatchType = "from"
	MismatchAttachments  MismatchType = "attachments"
	MismatchBody         MismatchType = "body"
	MismatchPythonNoFrom MismatchType = "python_no_from"
	MismatchEnmimeNoFrom MismatchType = "enmime_no_from"
)

type MismatchExample struct {
	MessageID   int64
	SourceMsgID string
	Type        MismatchType
	PythonValue string
	EnmimeValue string
}

func main() {
	var dbPath string
	var limit int
	var showExamples int

	flag.StringVar(&dbPath, "db", "", "Path to msgvault.db (default: ~/.msgvault/msgvault.db)")
	flag.IntVar(&limit, "limit", 100, "Number of messages to test")
	flag.IntVar(&showExamples, "examples", 5, "Number of examples to show per mismatch type")
	flag.Parse()

	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".msgvault", "msgvault.db")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		logger.Error("failed to open database", "error", err, "path", dbPath)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// Get messages with raw MIME data
	messages, err := loadMessages(ctx, db, limit)
	if err != nil {
		logger.Error("failed to load messages", "error", err)
		_ = db.Close() // close before exit so the deferred close isn't skipped
		os.Exit(1)     //nolint:gocritic // db is closed above
	}

	logger.Info("loaded messages", "count", len(messages))

	// Stats
	var (
		parseErrors    int
		perfectMatches int
		total          int
	)

	// Mismatch tracking
	mismatchCounts := make(map[MismatchType]int)
	mismatchExamples := make(map[MismatchType][]MismatchExample)

	// Field-level stats
	subjectMatches := 0
	fromMatches := 0
	attachmentMatches := 0
	bodyMatches := 0

	for _, msg := range messages {
		total++

		if total%10000 == 0 {
			logger.Info("progress", "processed", total)
		}

		// Load raw MIME
		rawMime, err := loadRawMime(ctx, db, msg.ID)
		if err != nil {
			continue
		}

		// Parse with enmime
		result := parseWithEnmime(rawMime)
		if result.Error != nil {
			parseErrors++
			continue
		}

		// Compare each field
		mismatches := []MismatchType{}

		// Subject
		if normalizeString(msg.Subject.String) == normalizeString(result.Subject) {
			subjectMatches++
		} else {
			mismatches = append(mismatches, MismatchSubject)
			addExample(mismatchExamples, MismatchSubject, msg, msg.Subject.String, result.Subject, showExamples)
		}

		// From address
		pythonFrom := ""
		enmimeFrom := ""
		if len(msg.FromAddresses) > 0 {
			pythonFrom = msg.FromAddresses[0].Email
		}
		if len(result.From) > 0 {
			enmimeFrom = result.From[0].Email
		}

		if normalizeEmail(pythonFrom) == normalizeEmail(enmimeFrom) {
			fromMatches++
		} else {
			// Categorize the from mismatch
			if pythonFrom == "" && enmimeFrom != "" {
				mismatches = append(mismatches, MismatchPythonNoFrom)
				addExample(mismatchExamples, MismatchPythonNoFrom, msg,
					fmt.Sprintf("(empty) python_from_count=%d", len(msg.FromAddresses)),
					fmt.Sprintf("%s <%s>", result.From[0].Name, result.From[0].Email),
					showExamples)
			} else if pythonFrom != "" && enmimeFrom == "" {
				mismatches = append(mismatches, MismatchEnmimeNoFrom)
				addExample(mismatchExamples, MismatchEnmimeNoFrom, msg, pythonFrom, "(empty)", showExamples)
			} else {
				mismatches = append(mismatches, MismatchFrom)
				addExample(mismatchExamples, MismatchFrom, msg, pythonFrom, enmimeFrom, showExamples)
			}
		}

		// Attachments
		if msg.AttachmentCount == result.Attachments {
			attachmentMatches++
		} else {
			mismatches = append(mismatches, MismatchAttachments)
			addExample(mismatchExamples, MismatchAttachments, msg,
				strconv.Itoa(msg.AttachmentCount),
				strconv.Itoa(result.Attachments),
				showExamples)
		}

		// Body (presence check)
		pythonHasBody := len(strings.TrimSpace(msg.BodyText.String)) > 0
		enmimeHasBody := len(strings.TrimSpace(result.BodyText)) > 0
		if pythonHasBody == enmimeHasBody {
			bodyMatches++
		} else {
			mismatches = append(mismatches, MismatchBody)
			pythonBodyLen := len(msg.BodyText.String)
			enmimeBodyLen := len(result.BodyText)
			addExample(mismatchExamples, MismatchBody, msg,
				fmt.Sprintf("len=%d has=%v", pythonBodyLen, pythonHasBody),
				fmt.Sprintf("len=%d has=%v", enmimeBodyLen, enmimeHasBody),
				showExamples)
		}

		// Count mismatches
		for _, mt := range mismatches {
			mismatchCounts[mt]++
		}

		if len(mismatches) == 0 {
			perfectMatches++
		}
	}

	// Summary
	fmt.Printf("\n=== ENMIME ANALYSIS (%d messages) ===\n\n", total)

	fmt.Printf("Parse errors: %d (%.2f%%)\n", parseErrors, pct(parseErrors, total))
	fmt.Printf("Perfect matches: %d (%.2f%%)\n", perfectMatches, pct(perfectMatches, total))

	fmt.Printf("\n--- Field-Level Match Rates ---\n")
	fmt.Printf("Subject:     %d / %d (%.2f%%)\n", subjectMatches, total-parseErrors, pct(subjectMatches, total-parseErrors))
	fmt.Printf("From:        %d / %d (%.2f%%)\n", fromMatches, total-parseErrors, pct(fromMatches, total-parseErrors))
	fmt.Printf("Attachments: %d / %d (%.2f%%)\n", attachmentMatches, total-parseErrors, pct(attachmentMatches, total-parseErrors))
	fmt.Printf("Body:        %d / %d (%.2f%%)\n", bodyMatches, total-parseErrors, pct(bodyMatches, total-parseErrors))

	fmt.Printf("\n--- Mismatch Breakdown ---\n")
	for _, mt := range []MismatchType{MismatchSubject, MismatchPythonNoFrom, MismatchEnmimeNoFrom, MismatchFrom, MismatchAttachments, MismatchBody} {
		count := mismatchCounts[mt]
		if count > 0 {
			fmt.Printf("%-20s: %d (%.2f%%)\n", mt, count, pct(count, total-parseErrors))
		}
	}

	fmt.Printf("\n--- Examples of Each Mismatch Type ---\n")
	for _, mt := range []MismatchType{MismatchSubject, MismatchPythonNoFrom, MismatchEnmimeNoFrom, MismatchFrom, MismatchAttachments, MismatchBody} {
		examples := mismatchExamples[mt]
		if len(examples) > 0 {
			fmt.Printf("\n[%s] (%d total)\n", mt, mismatchCounts[mt])
			for i, ex := range examples {
				if i >= showExamples {
					break
				}
				fmt.Printf("  msg_id=%d source=%s\n", ex.MessageID, ex.SourceMsgID)
				fmt.Printf("    Python: %s\n", truncate(ex.PythonValue, 100))
				fmt.Printf("    Enmime: %s\n", truncate(ex.EnmimeValue, 100))
			}
		}
	}
}

func addExample(examples map[MismatchType][]MismatchExample, mt MismatchType, msg PythonMessage, pythonVal, enmimeVal string, maxExamples int) {
	if len(examples[mt]) < maxExamples {
		examples[mt] = append(examples[mt], MismatchExample{
			MessageID:   msg.ID,
			SourceMsgID: msg.SourceMsgID,
			Type:        mt,
			PythonValue: pythonVal,
			EnmimeValue: enmimeVal,
		})
	}
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

func loadMessages(ctx context.Context, db *sql.DB, limit int) ([]PythonMessage, error) {
	query := `
		SELECT
			m.id,
			m.source_message_id,
			m.subject,
			mb.body_text,
			m.sent_at,
			(SELECT COUNT(*) FROM attachments a WHERE a.message_id = m.id) as attachment_count
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE EXISTS (SELECT 1 FROM message_raw mr WHERE mr.message_id = m.id)
		ORDER BY m.id
		LIMIT ?
	`

	rows, err := db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []PythonMessage
	for rows.Next() {
		var msg PythonMessage
		if err := rows.Scan(&msg.ID, &msg.SourceMsgID, &msg.Subject, &msg.BodyText, &msg.SentAt, &msg.AttachmentCount); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}

		msg.FromAddresses, _ = loadParticipants(ctx, db, msg.ID, "from")
		msg.ToAddresses, _ = loadParticipants(ctx, db, msg.ID, "to")
		msg.CcAddresses, _ = loadParticipants(ctx, db, msg.ID, "cc")
		msg.BccAddresses, _ = loadParticipants(ctx, db, msg.ID, "bcc")

		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

func loadParticipants(ctx context.Context, db *sql.DB, messageID int64, recipientType string) ([]Address, error) {
	// All recipient types (from, to, cc, bcc) are stored in message_recipients
	query := `
		SELECT COALESCE(p.display_name, ''), COALESCE(p.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = ?
	`

	rows, err := db.QueryContext(ctx, query, messageID, recipientType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var addresses []Address
	for rows.Next() {
		var addr Address
		if err := rows.Scan(&addr.Name, &addr.Email); err != nil {
			return nil, err
		}
		addresses = append(addresses, addr)
	}

	return addresses, rows.Err()
}

func loadRawMime(ctx context.Context, db *sql.DB, messageID int64) ([]byte, error) {
	var compressed []byte
	var compression sql.NullString

	err := db.QueryRowContext(
		ctx,
		"SELECT raw_data, compression FROM message_raw WHERE message_id = ?",
		messageID,
	).Scan(&compressed, &compression)
	if err != nil {
		return nil, err
	}

	// Decompress if needed
	if compression.Valid && compression.String == "zlib" {
		r, err := zlib.NewReader(bytes.NewReader(compressed))
		if err != nil {
			return nil, fmt.Errorf("zlib reader: %w", err)
		}
		defer func() { _ = r.Close() }()

		return io.ReadAll(r)
	}

	return compressed, nil
}

func parseWithEnmime(raw []byte) ParseResult {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return ParseResult{Error: err}
	}

	result := ParseResult{
		Subject:     env.GetHeader("Subject"),
		BodyText:    env.Text,
		Attachments: len(env.Attachments),
	}

	// Parse addresses using env.AddressList which handles edge cases better
	result.From = parseEnmimeAddressList(env, "From")
	result.To = parseEnmimeAddressList(env, "To")
	result.Cc = parseEnmimeAddressList(env, "Cc")
	result.Bcc = parseEnmimeAddressList(env, "Bcc")

	return result
}

func parseEnmimeAddressList(env *enmime.Envelope, header string) []Address {
	var addresses []Address
	list, err := env.AddressList(header)
	if err != nil {
		return addresses
	}
	for _, addr := range list {
		addresses = append(addresses, Address{
			Name:  addr.Name,
			Email: addr.Address,
		})
	}
	return addresses
}

func normalizeString(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}

func normalizeEmail(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}
