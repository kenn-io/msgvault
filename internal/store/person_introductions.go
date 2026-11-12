package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/sqliteutil"
)

var (
	ErrIntroductionNotFound   = errors.New("introduction not found")
	ErrIntroductionInvalid    = errors.New("invalid introduction")
	ErrIntroductionSamePerson = errors.New("same person")
	ErrIntroductionRevision   = errors.New("introduction revision conflict")
	ErrIntroductionState      = errors.New("introduction state conflict")
	ErrIntroductionSource     = errors.New("unsupported introduction source")
)

type PersonIntroduction struct {
	IntroID          string     `json:"intro_id"`
	Revision         int64      `json:"revision"`
	Status           string     `json:"status"`
	SourceID         int64      `json:"source_id"`
	PersonAID        int64      `json:"person_a_id"`
	PersonBID        int64      `json:"person_b_id"`
	Title            string     `json:"title"`
	Body             string     `json:"body"`
	CreatedBy        string     `json:"created_by"`
	CancelledAt      *time.Time `json:"cancelled_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	SourceType       string     `json:"-"`
	SourceIdentifier string     `json:"-"`
}
type PersonIntroductionPrepare struct {
	SourceID  int64  `json:"source_id"`
	PersonAID int64  `json:"person_a_id"`
	PersonBID int64  `json:"person_b_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}
type PersonIntroductionEdit struct {
	Revision int64   `json:"revision"`
	Title    *string `json:"title"`
	Body     *string `json:"body"`
}
type PersonIntroductionQuery struct {
	SourceID int64  `json:"source_id"`
	Limit    int    `json:"limit"`
	AfterID  string `json:"after_id"`
}
type PersonIntroductionPage struct {
	Items       []PersonIntroduction `json:"items"`
	HasMore     bool                 `json:"has_more"`
	NextAfterID string               `json:"next_after_id"`
}

const introductionCursorBytes = 2 << 10

type introductionCursor struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"i"`
}

const introductionColumns = `i.intro_id, i.revision, i.status, i.source_id, i.person_a_id, i.person_b_id, i.title, i.body, i.created_by, i.cancelled_at, i.created_at, i.updated_at, s.source_type, s.identifier`

func scanIntroduction(row interface{ Scan(dest ...any) error }) (PersonIntroduction, error) {
	var v PersonIntroduction
	var body sql.NullString
	err := row.Scan(&v.IntroID, &v.Revision, &v.Status, &v.SourceID, &v.PersonAID, &v.PersonBID, &v.Title, &body, &v.CreatedBy, &v.CancelledAt, &v.CreatedAt, &v.UpdatedAt, &v.SourceType, &v.SourceIdentifier)
	v.Body = body.String
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrIntroductionNotFound
	}
	return v, err
}
func validIntroText(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }
func (s *Store) PreparePersonIntroductionContext(ctx context.Context, in PersonIntroductionPrepare, actor string) (PersonIntroduction, error) {
	var result PersonIntroduction
	if in.SourceID <= 0 || in.PersonAID <= 0 || in.PersonBID <= 0 || !validIntroText(in.Title) || !validIntroText(in.Body) || actor == "" {
		return result, ErrIntroductionInvalid
	}
	if in.PersonAID == in.PersonBID {
		return result, ErrIntroductionSamePerson
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return result, fmt.Errorf("generate introduction id: %w", err)
	}
	id := "intro-" + hex.EncodeToString(random[:])
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var typ string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, in.SourceID).Scan(&typ); errors.Is(err, sql.ErrNoRows) {
			return ErrIntroductionSource
		} else if err != nil {
			return err
		}
		if typ != sourceTypeBeeper {
			return ErrIntroductionSource
		}
		if err := requireSourceWritableWith(tx, in.SourceID); err != nil {
			return err
		}
		if err := s.requirePersonsExistTx(ctx, tx, in.PersonAID, in.PersonBID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_introductions (intro_id, revision, status, source_id, person_a_id, person_b_id, title, body, created_by) VALUES (?, 1, 'prepared', ?, ?, ?, ?, ?, ?)`, id, in.SourceID, in.PersonAID, in.PersonBID, in.Title, in.Body, actor); err != nil {
			return err
		}
		var err error
		result, err = scanIntroduction(tx.QueryRowContext(ctx, `SELECT `+introductionColumns+` FROM person_introductions i JOIN sources s ON s.id = i.source_id WHERE i.intro_id = ?`, id))
		return err
	})
	return result, err
}
func (s *Store) GetPersonIntroductionContext(ctx context.Context, id string) (PersonIntroduction, error) {
	return scanIntroduction(s.db.QueryRowContext(ctx, `SELECT `+introductionColumns+` FROM person_introductions i JOIN sources s ON s.id = i.source_id WHERE i.intro_id = ?`, id))
}
func (s *Store) ListPersonIntroductionsContext(ctx context.Context, q PersonIntroductionQuery, allowed []int64) (PersonIntroductionPage, error) {
	page := PersonIntroductionPage{Items: []PersonIntroduction{}}
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 100 || q.SourceID < 0 {
		return page, ErrIntroductionInvalid
	}
	if allowed != nil && len(allowed) == 0 {
		return page, nil
	}
	query := `SELECT ` + introductionColumns + ` FROM person_introductions i JOIN sources s ON s.id = i.source_id WHERE 1=1`
	args := []any{}
	if q.SourceID > 0 {
		query += ` AND i.source_id = ?`
		args = append(args, q.SourceID)
	}
	if allowed != nil {
		query += ` AND i.source_id IN (` + strings.TrimRight(strings.Repeat("?,", len(allowed)), ",") + `)`
		for _, id := range allowed {
			args = append(args, id)
		}
	}
	if q.AfterID != "" {
		if len(q.AfterID) > base64.RawURLEncoding.EncodedLen(introductionCursorBytes) {
			return page, ErrIntroductionInvalid
		}
		raw, err := base64.RawURLEncoding.DecodeString(q.AfterID)
		var c introductionCursor
		if err != nil || len(raw) > introductionCursorBytes {
			return page, ErrIntroductionInvalid
		}
		decoder := jsontext.NewDecoder(strings.NewReader(string(raw)))
		if json.UnmarshalDecode(decoder, &c, json.RejectUnknownMembers(true)) != nil {
			return page, ErrIntroductionInvalid
		}
		if _, err := decoder.ReadValue(); !errors.Is(err, io.EOF) || c.Version != 1 || c.ID == "" || c.CreatedAt.IsZero() {
			return page, ErrIntroductionInvalid
		}
		var position any = c.CreatedAt
		if s.IsPostgreSQL() {
			query += ` AND (i.created_at < ? OR (i.created_at = ? AND i.intro_id < ?))`
		} else {
			key := sqliteutil.TimestampKeyFunction + `(i.created_at)`
			query += ` AND (` + key + ` < ? OR (` + key + ` = ? AND i.intro_id < ?))`
			position = sqliteutil.TimestampKey(c.CreatedAt)
		}
		args = append(args, position, position, c.ID)
	}
	order := "i.created_at"
	if !s.IsPostgreSQL() {
		order = sqliteutil.TimestampKeyFunction + "(i.created_at)"
	}
	query += ` ORDER BY ` + order + ` DESC, i.intro_id DESC LIMIT ?`
	args = append(args, q.Limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		v, err := scanIntroduction(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, v)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > q.Limit {
		page.HasMore = true
		page.Items = page.Items[:q.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextAfterID, err = IntroductionCursor(last)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

// IntroductionCursor encodes the position of an introduction in list order.
func IntroductionCursor(row PersonIntroduction) (string, error) {
	raw, err := json.Marshal(introductionCursor{Version: 1, CreatedAt: row.CreatedAt, ID: row.IntroID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func (s *Store) EditPersonIntroductionContext(ctx context.Context, id string, in PersonIntroductionEdit) (PersonIntroduction, error) {
	if in.Revision <= 0 || (in.Title == nil && in.Body == nil) || (in.Title != nil && !validIntroText(*in.Title)) || (in.Body != nil && !validIntroText(*in.Body)) {
		return PersonIntroduction{}, ErrIntroductionInvalid
	}
	return s.updateIntroduction(ctx, id, in.Revision, in.Title, in.Body, false)
}
func (s *Store) CancelPersonIntroductionContext(ctx context.Context, id string, revision int64) (PersonIntroduction, error) {
	if revision <= 0 {
		return PersonIntroduction{}, ErrIntroductionInvalid
	}
	return s.updateIntroduction(ctx, id, revision, nil, nil, true)
}
func (s *Store) updateIntroduction(ctx context.Context, id string, revision int64, title, body *string, cancel bool) (PersonIntroduction, error) {
	var result PersonIntroduction
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		row, err := scanIntroduction(tx.QueryRowContext(ctx, `SELECT `+introductionColumns+` FROM person_introductions i JOIN sources s ON s.id = i.source_id WHERE i.intro_id = ?`, id))
		if err != nil {
			return err
		}
		if row.Revision != revision {
			return ErrIntroductionRevision
		}
		if row.Status != "prepared" {
			return ErrIntroductionState
		}
		var updated sql.Result
		if cancel {
			updated, err = tx.ExecContext(ctx, `UPDATE person_introductions SET status = 'cancelled', body = NULL, cancelled_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP, revision = revision + 1 WHERE intro_id = ? AND revision = ? AND status = 'prepared'`, id, revision)
		} else {
			if err := requireSourceWritableWith(tx, row.SourceID); err != nil {
				return err
			}
			if title == nil {
				title = &row.Title
			}
			if body == nil {
				body = &row.Body
			}
			updated, err = tx.ExecContext(ctx, `UPDATE person_introductions SET title = ?, body = ?, updated_at = CURRENT_TIMESTAMP, revision = revision + 1 WHERE intro_id = ? AND revision = ? AND status = 'prepared'`, *title, *body, id, revision)
		}
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrIntroductionState
		}
		result, err = scanIntroduction(tx.QueryRowContext(ctx, `SELECT `+introductionColumns+` FROM person_introductions i JOIN sources s ON s.id = i.source_id WHERE i.intro_id = ?`, id))
		return err
	})
	return result, err
}
