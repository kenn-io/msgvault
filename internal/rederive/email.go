package rederive

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Bump the version when parsing the same retained MIME produces different text.
const emailVersion = "v1"
const emailBatchSize = 500

func init() {
	for _, sourceType := range []string{"gmail", "imap", "msmail", "o365", "mbox", "hey", "google-groups", "apple-mail", "pst", "eml", "maildir"} {
		Register(sourceType, emailVersion, repairEmail)
	}
}

// repairEmail rebuilds text from retained MIME, including Gmail Chat messages.
// The cursor advances only past committed or undecodable rows, so interrupted
// syncs can finish large archives over several attempts.
func repairEmail(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (sum *Summary, err error) {
	start := time.Now()
	sum = &Summary{}
	defer func() { sum.Duration = time.Since(start) }()
	src, err := s.GetSourceByID(sourceID)
	if err != nil {
		return sum, err
	}
	var resume struct {
		Version string                    `json:"email_rederive_version,omitempty"`
		AfterID int64                     `json:"email_rederive_after_id,omitzero"`
		Other   map[string]jsontext.Value `json:",embed"`
	}
	if src.SyncConfig.Valid {
		if err := json.Unmarshal([]byte(src.SyncConfig.String), &resume); err != nil {
			return sum, fmt.Errorf("read email repair checkpoint: %w", err)
		}
	}
	if resume.Version != emailVersion {
		resume.Version, resume.AfterID = emailVersion, 0
	}
	afterID := resume.AfterID
	checkpoint := func() error {
		saved := resume
		saved.AfterID = afterID
		blob, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := s.UpdateSourceSyncConfig(sourceID, string(blob)); err != nil {
			return err
		}
		resume = saved
		return nil
	}
	defer func() {
		if err != nil && afterID > resume.AfterID {
			err = errors.Join(err, checkpoint())
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		batch, err := s.ScanArchivedRawMessagesContext(ctx, sourceID, "mime", afterID, emailBatchSize)
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
			snippet := textutil.TruncateRunes(textutil.FirstLine(strings.TrimSpace(body)), 200)
			if err := s.UpdateMessageDerivedEmailTextContext(ctx, item.MessageID,
				sql.NullString{String: body, Valid: body != ""}, sql.NullString{String: html, Valid: html != ""},
				sql.NullString{String: snippet, Valid: snippet != ""}); err != nil {
				sum.Errors++
				return sum, fmt.Errorf("rebuild email %d: %w", item.MessageID, err)
			}
			if item.BodyText != body {
				sum.BodiesRewritten++
			}
			afterID = item.MessageID
		}
		if err := checkpoint(); err != nil {
			return sum, err
		}
		if progress != nil {
			progress(fmt.Sprintf("%d scanned, %d bodies rewritten", sum.MessagesScanned, sum.BodiesRewritten))
		}
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	resume.Version, afterID = "", 0
	return sum, checkpoint()
}
