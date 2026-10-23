package rederive

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Bump the version when parsing the same retained MIME produces different text.
const emailVersion = "email-v1"

// repairEmail rebuilds text from retained MIME, including Gmail Chat messages.
// Each attempt starts at the first row; completed writes survive interruption.
func repairEmail(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (*Summary, error) {
	start := time.Now()
	sum := &Summary{unchanged: true}
	defer func() { sum.Duration = time.Since(start) }()
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		batch, err := s.ScanArchivedRawMessagesContext(ctx, sourceID, "mime", afterID, 1)
		if err != nil {
			return sum, err
		}
		if len(batch) == 0 {
			break
		}
		item := batch[0]
		afterID = item.MessageID
		sum.MessagesScanned++
		parsed, err := mime.Parse(item.RawData)
		if err != nil {
			sum.Undecodable++
			continue
		}
		body := textutil.EnsureUTF8(parsed.GetBodyText())
		html := textutil.EnsureUTF8(parsed.BodyHTML)
		snippet := item.Snippet
		if snippet != "" && snippet == textutil.SnippetFromBody(item.BodyText) {
			snippet = textutil.SnippetFromBody(body)
		}
		changed, err := s.UpdateMessageDerivedEmailTextContext(ctx, item.MessageID,
			sql.NullString{String: body, Valid: body != ""}, sql.NullString{String: html, Valid: html != ""},
			sql.NullString{String: snippet, Valid: snippet != ""})
		if err != nil {
			if ctx.Err() != nil {
				return sum, ctx.Err()
			}
			sum.Errors++
			continue
		}
		sum.unchanged = sum.unchanged && !changed
		if changed && item.BodyText != body {
			sum.BodiesRewritten++
		}
		if progress != nil {
			progress(fmt.Sprintf("%d scanned, %d bodies rewritten", sum.MessagesScanned, sum.BodiesRewritten))
		}
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	return sum, nil
}
