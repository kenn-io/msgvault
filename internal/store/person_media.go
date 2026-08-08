package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonMediaKind string

const (
	PersonMediaPhoto PersonMediaKind = "photo"
	PersonMediaLogo  PersonMediaKind = "logo"
	PersonMediaSound PersonMediaKind = "sound"
	PersonMediaKey   PersonMediaKind = "key"
)

func (k PersonMediaKind) Valid() bool {
	switch k {
	case PersonMediaPhoto, PersonMediaLogo, PersonMediaSound, PersonMediaKey:
		return true
	default:
		return false
	}
}

const MaxPersonMediaBytes = 8 << 20

type PersonMedia struct {
	Envelope      ValueEnvelope   `json:"envelope"`
	PersonID      int64           `json:"person_id"`
	MediaKind     PersonMediaKind `json:"media_kind"`
	MediaType     *string         `json:"media_type,omitempty"`
	URI           *string         `json:"uri,omitempty"`
	ByteSize      *int64          `json:"byte_size,omitempty"`
	ContentHash   *string         `json:"content_hash,omitempty"`
	HasData       bool            `json:"has_data"`
	OriginalValue string          `json:"original_value"`
}

type PersonMediaInput struct {
	MediaKind     PersonMediaKind `json:"media_kind"`
	MediaType     *string         `json:"media_type,omitempty"`
	URI           *string         `json:"uri,omitempty"`
	Data          []byte          `json:"data,omitempty"`
	OriginalValue string          `json:"original_value"`
	Envelope      ValueEnvelope   `json:"envelope"`
}

var (
	ErrInvalidPersonMediaKind = errors.New("invalid person media kind")
	ErrPersonMediaEmpty       = errors.New("person media requires inline data or a URI")
	ErrPersonMediaTooLarge    = errors.New("person media exceeds the maximum inline size")
	ErrPersonMediaNoData      = errors.New("person media row has no inline data")
)

func (s *Store) AddPersonMediaContext(
	ctx context.Context, personID int64, input PersonMediaInput,
) (*PersonMedia, error) {
	var result *PersonMedia
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonMediaTx(ctx, tx, personID, input)
		if err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) ListPersonMediaContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonMedia, error) {
	var media []PersonMedia
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		media, err = s.listPersonMediaTx(ctx, tx, personID, currentOnly)
		return err
	})
	return media, err
}

func (s *Store) ReadPersonMediaDataContext(
	ctx context.Context, personID, mediaID int64,
) ([]byte, string, error) {
	var data []byte
	var mediaType sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT data, media_type FROM person_media WHERE person_id = ? AND id = ?`,
		personID, mediaID,
	).Scan(&data, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrProfileValueNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("read person media data: %w", err)
	}
	if len(data) == 0 {
		return nil, "", ErrPersonMediaNoData
	}
	return data, mediaType.String, nil
}

func (s *Store) SupersedePersonMediaContext(
	ctx context.Context, personID, mediaID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.supersedePersonMediaTx(ctx, tx, personID, mediaID, activeUntil); err != nil {
			return err
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func (s *Store) addPersonMediaTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonMediaInput,
) (*PersonMedia, error) {
	if !input.MediaKind.Valid() {
		return nil, ErrInvalidPersonMediaKind
	}
	if err := input.Envelope.Validate(); err != nil {
		return nil, err
	}
	hasURI := input.URI != nil && strings.TrimSpace(*input.URI) != ""
	if len(input.Data) == 0 && !hasURI {
		return nil, ErrPersonMediaEmpty
	}
	if len(input.Data) > MaxPersonMediaBytes {
		return nil, ErrPersonMediaTooLarge
	}
	original := input.OriginalValue
	if strings.TrimSpace(original) == "" && hasURI {
		original = strings.TrimSpace(*input.URI)
	}
	var data, byteSize, contentHash any
	if len(input.Data) > 0 {
		digest := sha256.Sum256(input.Data)
		data, byteSize = input.Data, int64(len(input.Data))
		contentHash = hex.EncodeToString(digest[:])
	}
	env := input.Envelope
	var err error
	if env.Ordinal == 0 {
		env.Ordinal, err = nextProfileOrdinalTx(
			ctx, tx, "person_media", "media_kind", personID, input.MediaKind,
		)
		if err != nil {
			return nil, err
		}
	}
	args := []any{
		personID, input.MediaKind, stringValue(input.MediaType),
		stringValue(input.URI), data, byteSize, contentHash, original,
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_media (
		person_id, media_kind, media_type, uri, data, byte_size,
		content_hash, original_value, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person media: %w", err)
	}
	return getPersonMediaTx(ctx, tx, personID, id)
}

func (s *Store) listPersonMediaTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonMedia, error) {
	query := personMediaSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	query += ` ORDER BY media_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonMedia, personID)
}

func (s *Store) supersedePersonMediaTx(
	ctx context.Context, tx *loggedTx, personID, mediaID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_media", personID, mediaID, activeUntil,
	)
}

const personMediaSelect = `SELECT
	id, person_id, media_kind, media_type, uri, byte_size, content_hash,
	(data IS NOT NULL) AS has_data, original_value, ` + profileEnvelopeReadColumns + `
	FROM person_media`

func getPersonMediaTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonMedia, error) {
	media, err := scanPersonMedia(tx.QueryRowContext(ctx,
		personMediaSelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return media, err
}

func scanPersonMedia(row scanner) (*PersonMedia, error) {
	var media PersonMedia
	var mediaType, uri, contentHash sql.NullString
	var byteSize sql.NullInt64
	var env profileEnvelopeScanValues
	dest := []any{
		&media.Envelope.ID, &media.PersonID, &media.MediaKind, &mediaType,
		&uri, &byteSize, &contentHash, &media.HasData, &media.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	media.MediaType = nullStringPtr(mediaType)
	media.URI = nullStringPtr(uri)
	if byteSize.Valid {
		media.ByteSize = &byteSize.Int64
	}
	media.ContentHash = nullStringPtr(contentHash)
	if err := env.apply(&media.Envelope); err != nil {
		return nil, err
	}
	return &media, nil
}
