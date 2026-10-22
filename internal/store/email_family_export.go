package store

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"time"

	internalmime "go.kenn.io/msgvault/internal/mime"
)

// EmailFamilyExportManifest records the exact archive snapshot and raw outcomes.
type EmailFamilyExportManifest struct {
	Revision    string                    `json:"revision"`
	GeneratedAt time.Time                 `json:"generated_at"`
	Complete    bool                      `json:"complete"`
	Truncated   bool                      `json:"truncated"`
	Members     []EmailFamilyExportMember `json:"members"`
}

type EmailFamilyExportMember struct {
	EmailFamilyMember

	RawFormat string `json:"raw_format"`
	Filename  string `json:"filename"`
	SHA256    string `json:"sha256"`
	State     string `json:"state"`
}

type emailFamilyExportWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w emailFamilyExportWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}

// ExportEmailFamilyContext validates the revision before writing a ZIP. All facts
// and originals come from one snapshot; at most one decoded original is held.
// Callers serving HTTP must stage output so storage failures precede headers.
func (s *Store) ExportEmailFamilyContext(ctx context.Context, id int64, expectedRevision string, options EmailFamilyOptions, output io.Writer) (*EmailFamilyExportManifest, error) {
	ctx = emailFamilyScope(ctx, options.SourceIDs)
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	family, err := s.emailFamilySnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if expectedRevision == "" || family.Revision != expectedRevision {
		return nil, ErrEmailFamilyChanged
	}
	manifest := &EmailFamilyExportManifest{Revision: family.Revision, GeneratedAt: time.Now().UTC(), Complete: !family.Truncated, Truncated: family.Truncated, Members: []EmailFamilyExportMember{}}
	archive := zip.NewWriter(emailFamilyExportWriter{ctx, output})
	for i, member := range family.Members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := EmailFamilyExportMember{EmailFamilyMember: member, State: "missing_raw"}
		var stored []byte
		var compression sql.NullString
		var encryption sql.NullInt64
		var storedBytes int64
		err := ReadDBContext(ctx, s.db.DB).QueryRowContext(ctx, s.Rebind(`SELECT CASE WHEN length(raw_data)<=? THEN raw_data ELSE NULL END,raw_format,compression,encryption_version,length(raw_data) FROM message_raw WHERE message_id=?`), internalmime.EmailLinkageMaxBytes+(1<<20), member.MessageID).Scan(&stored, &entry.RawFormat, &compression, &encryption, &storedBytes)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("read family original: %w", err)
		case encryption.Int64 != 0:
			entry.State = "encrypted_raw"
		case storedBytes > internalmime.EmailLinkageMaxBytes+(1<<20):
			entry.State = "undecodable"
		case entry.RawFormat != emailLinkageRawFormat:
			entry.State = "undecodable"
		case compression.String != "" && compression.String != "none" && compression.String != "zlib":
			entry.State = "undecodable"
		default:
			raw, _, decodeErr := decodeMessageRawBounded(stored, compression, internalmime.EmailLinkageMaxBytes, errEmailLinkageRawTooLarge)
			if decodeErr != nil {
				entry.State = "undecodable"
				break
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			entry.State = "exported"
			entry.Filename = fmt.Sprintf("copies/%d.eml", i+1)
			entry.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
			writer, err := archive.Create(entry.Filename)
			if err != nil {
				return nil, fmt.Errorf("create family ZIP member: %w", err)
			}
			if _, err := writer.Write(raw); err != nil {
				return nil, fmt.Errorf("write family original: %w", err)
			}
		}
		if entry.State != "exported" {
			manifest.Complete = false
		}
		manifest.Members = append(manifest.Members, entry)
	}
	data, err := json.Marshal(manifest, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return nil, err
	}
	writer, err := archive.Create("manifest.json")
	if err != nil {
		return nil, fmt.Errorf("create family ZIP manifest: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("finish family ZIP: %w", err)
	}
	return manifest, nil
}
