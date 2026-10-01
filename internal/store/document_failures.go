package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const maxDocumentFailureDetailBytes = 1024
const documentFailureStatusLimit = 20

// DocumentFailureDiagnostic identifies a current failed owner without exposing
// provider responses or extracted text.
type DocumentFailureDiagnostic struct {
	CanonicalBlobHash string `json:"canonical_blob_hash"`
	ReasonCode        string `json:"reason_code"`
	Detail            string `json:"detail" required:"false"`
	State             string `json:"state"`
}

// CleanDocumentFailureDetail bounds a diagnostic for durable and terminal use.
// Callers must select content-safe errors before supplying their text.
func CleanDocumentFailureDetail(value string) string {
	value, _, _ = strings.Cut(value, "\n")
	value, _, _ = strings.Cut(value, "\r")
	value = ansi.Strip(value)
	var out strings.Builder
	for _, char := range value {
		if char == utf8.RuneError || unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			continue
		}
		if out.Len()+utf8.RuneLen(char) > maxDocumentFailureDetailBytes {
			break
		}
		out.WriteRune(char)
	}
	return strings.TrimSpace(out.String())
}

func (s *Store) documentFailureDiagnostics(ctx context.Context, profileID, inputKey string, mediaTypes, messageTypes []string) ([]DocumentFailureDiagnostic, bool, error) {
	scopeSQL, args, err := documentOccurrenceScopeSQL(profileID, "o", "m", mediaTypes, messageTypes)
	if err != nil {
		return nil, false, err
	}
	args = append(args, profileID, inputKey, documentFailureStatusLimit+1)
	rows, err := s.db.QueryContext(ctx, s.Rebind(`
  WITH eligible AS (
   SELECT o.canonical_blob_hash FROM document_occurrences o
   JOIN messages m ON m.id = o.message_id
   WHERE `+scopeSQL+`
   GROUP BY o.canonical_blob_hash
  ), attempts AS (
   SELECT e.*, ROW_NUMBER() OVER (PARTITION BY e.canonical_blob_hash ORDER BY e.created_at DESC, e.id DESC) AS owner_rank
   FROM document_extractions e JOIN eligible o ON o.canonical_blob_hash = e.canonical_blob_hash
   WHERE e.profile_id = ? AND e.extraction_input_key = ?
  )
  SELECT x.canonical_blob_hash, COALESCE(x.failure_reason,x.terminal_reason,''), COALESCE(x.failure_detail,''), x.state
  FROM attempts x
  WHERE x.owner_rank=1 AND x.state IN ('terminal','tombstoned')
   AND COALESCE(x.failure_reason,x.terminal_reason,'') <> ''
  ORDER BY x.created_at DESC, x.id DESC LIMIT ?`), args...)
	if err != nil {
		return nil, false, fmt.Errorf("list document failure diagnostics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	failures := make([]DocumentFailureDiagnostic, 0, documentFailureStatusLimit)
	for rows.Next() {
		var failure DocumentFailureDiagnostic
		if err := rows.Scan(&failure.CanonicalBlobHash, &failure.ReasonCode, &failure.Detail, &failure.State); err != nil {
			return nil, false, fmt.Errorf("read document failure diagnostic: %w", err)
		}
		failure.Detail = CleanDocumentFailureDetail(failure.Detail)
		failures = append(failures, failure)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate document failure diagnostics: %w", err)
	}
	exhausted := len(failures) <= documentFailureStatusLimit
	if !exhausted {
		failures = failures[:documentFailureStatusLimit]
	}
	return failures, exhausted, nil
}
