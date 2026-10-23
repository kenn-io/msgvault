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
	sum := &Summary{}
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
		for _, item := range batch {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			sum.MessagesScanned++
			parsed, err := mime.Parse(item.RawData)
			if err != nil {
				sum.Undecodable++
				afterID = item.MessageID
				continue
			}
			body := textutil.EnsureUTF8(parsed.GetBodyText())
			html := textutil.EnsureUTF8(parsed.BodyHTML)
			snippet := item.Snippet
			if snippet == textutil.SnippetFromBody(item.BodyText) {
				snippet = textutil.SnippetFromBody(body)
			}
			if err := s.UpdateMessageDerivedEmailTextContext(ctx, item.MessageID,
				sql.NullString{String: body, Valid: body != ""}, sql.NullString{String: html, Valid: html != ""},
				sql.NullString{String: snippet, Valid: snippet != ""}); err != nil {
				if ctx.Err() != nil {
					return sum, ctx.Err()
				}
				sum.Errors++
				afterID = item.MessageID
				continue
			}
			if item.BodyText != body {
				sum.BodiesRewritten++
			}
			afterID = item.MessageID
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
