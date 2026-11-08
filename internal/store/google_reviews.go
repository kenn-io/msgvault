package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/googlereviews"
	"go.kenn.io/msgvault/internal/search"
)

const googleReviewsPageUpsertSQL = `
INSERT INTO google_reviews (
	target_name, location_name, review_id, review_name,
	reviewer_display_name, reviewer_profile_photo_url, reviewer_is_anonymous,
	star_rating, comment, create_time, update_time,
	has_reply, reply_comment, reply_update_time, reply_state, reply_policy_violation, archived_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (target_name, location_name, review_id) DO UPDATE SET
	review_name = excluded.review_name,
	reviewer_display_name = excluded.reviewer_display_name,
	reviewer_profile_photo_url = excluded.reviewer_profile_photo_url,
	reviewer_is_anonymous = excluded.reviewer_is_anonymous,
	star_rating = excluded.star_rating,
	comment = excluded.comment,
	create_time = excluded.create_time,
	update_time = excluded.update_time,
	has_reply = excluded.has_reply,
	reply_comment = excluded.reply_comment,
	reply_update_time = excluded.reply_update_time,
	reply_state = excluded.reply_state,
	reply_policy_violation = excluded.reply_policy_violation,
	archived_at = excluded.archived_at`

const googleReviewTimestampFormat = "2006-01-02T15:04:05.000000000Z"

func (s *Store) BeginGoogleReviewsBackfill(ctx context.Context, target, location string) (googlereviews.BackfillState, error) {
	if !validGoogleReviewTarget(target, location) {
		return googlereviews.BackfillState{}, googlereviews.ErrInvalid
	}
	now := time.Now().UTC().Format(googleReviewTimestampFormat)
	var state googlereviews.BackfillState
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO google_reviews_sync_state (
				target_name, location_name, status, run_id, started_at, updated_at
			) VALUES (?, ?, 'running', 1, ?, ?)
			ON CONFLICT (target_name, location_name) DO NOTHING`, target, location, now, now)
		if err != nil {
			return fmt.Errorf("create Google Reviews backfill state: %w", err)
		}
		state, err = readGoogleReviewsBackfillState(ctx, tx, s.IsPostgreSQL(), target, location)
		if err != nil {
			return err
		}
		if state.Status == "api_exhausted" {
			if state.RunID == math.MaxInt64 {
				return googlereviews.ErrArchive
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM google_reviews_sync_tokens WHERE target_name = ? AND location_name = ?`, target, location)
			if err != nil {
				return fmt.Errorf("clear Google Reviews page-token history: %w", err)
			}
			state.RunID++
			state.PagesFetched = 0
			state.ReviewsFetched = 0
			state.TotalReviewCount = 0
			state.LastErrorCode = ""
			state.NextPageToken = ""
			state.StartedAt = timePointer(now)
			state.CompletedAt = nil
			state.UpdatedAt = timePointer(now)
			state.Status = "running"
		} else if state.Status == "partial" {
			if state.LastErrorCode == "cursor_cycle" {
				if state.RunID == math.MaxInt64 {
					return googlereviews.ErrArchive
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM google_reviews_sync_tokens WHERE target_name = ? AND location_name = ?`, target, location); err != nil {
					return fmt.Errorf("clear cyclic Google Reviews page-token history: %w", err)
				}
				// A repeated cursor cannot make progress by resuming at the same
				// page. An explicit retry starts a new traversal; archived rows
				// remain and page upserts are idempotent.
				state.RunID++
				state.NextPageToken = ""
				state.PagesFetched = 0
				state.ReviewsFetched = 0
				state.TotalReviewCount = 0
				state.StartedAt = timePointer(now)
			}
			state.Status = "running"
			state.LastErrorCode = ""
			state.CompletedAt = nil
			state.UpdatedAt = timePointer(now)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO google_reviews_sync_tokens(target_name, location_name, run_id, token_hash)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (target_name, location_name, run_id, token_hash) DO NOTHING`, target, location, state.RunID, googleReviewTokenHash(state.NextPageToken)); err != nil {
			return fmt.Errorf("record Google Reviews page-token history: %w", err)
		}
		state.ArchivedReviewCount, err = countGoogleReviews(ctx, tx, target, location)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE google_reviews_sync_state
			SET status = 'running', run_id = ?, next_page_token = ?, pages_fetched = ?, reviews_fetched = ?,
				total_review_count = ?, archived_review_count = ?, last_error_code = '', completed_at = NULL,
				started_at = ?, updated_at = ?
			WHERE target_name = ? AND location_name = ?`, state.RunID, state.NextPageToken, state.PagesFetched,
			state.ReviewsFetched, state.TotalReviewCount, state.ArchivedReviewCount, timeString(state.StartedAt, now), now, target, location); err != nil {
			return fmt.Errorf("resume Google Reviews backfill state: %w", err)
		}
		state.Target = target
		state.Location = location
		state.UpdatedAt = timePointer(now)
		return nil
	})
	if err != nil {
		return googlereviews.BackfillState{}, err
	}
	return state, nil
}

func (s *Store) ApplyGoogleReviewsPage(ctx context.Context, target, location, expectedPageToken string, page googlereviews.Page) (googlereviews.BackfillState, error) {
	if err := page.ValidateFor(target, location); err != nil {
		return googlereviews.BackfillState{}, err
	}
	now := time.Now().UTC().Format(googleReviewTimestampFormat)
	var state googlereviews.BackfillState
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		locked, err := readGoogleReviewsBackfillState(ctx, tx, s.IsPostgreSQL(), target, location)
		if err != nil {
			return err
		}
		if locked.Status != "running" || locked.NextPageToken != expectedPageToken {
			return googlereviews.ErrArchiveBusy
		}
		for _, review := range page.Reviews {
			name := review.Name
			if name == "" {
				name = location + "/reviews/" + review.ReviewID
			}
			hasReply := review.ReviewReply != nil
			reply := googlereviews.Reply{}
			if hasReply {
				reply = *review.ReviewReply
			}
			_, err := tx.ExecContext(ctx, googleReviewsPageUpsertSQL,
				target, location, review.ReviewID, name,
				review.Reviewer.DisplayName, review.Reviewer.ProfilePhotoURL, review.Reviewer.IsAnonymous,
				review.StarRating, review.Comment, canonicalReviewTime(review.CreateTime), canonicalReviewTime(review.UpdateTime),
				hasReply, reply.Comment, canonicalReviewTime(reply.UpdateTime), reply.ReviewReplyState, reply.PolicyViolation, now,
			)
			if err != nil {
				return fmt.Errorf("upsert Google review page record: %w", err)
			}
		}
		if page.NextPageToken != "" {
			result, err := tx.ExecContext(ctx, `
				INSERT INTO google_reviews_sync_tokens(target_name, location_name, run_id, token_hash)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (target_name, location_name, run_id, token_hash) DO NOTHING`, target, location, locked.RunID, googleReviewTokenHash(page.NextPageToken))
			if err != nil {
				return fmt.Errorf("record next Google Reviews page token: %w", err)
			}
			inserted, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("check Google Reviews page-token history: %w", err)
			}
			if inserted == 0 {
				return googlereviews.ErrArchiveCycle
			}
		}
		archivedCount, err := countGoogleReviews(ctx, tx, target, location)
		if err != nil {
			return err
		}
		status := "running"
		var completed any
		if page.NextPageToken == "" {
			status = "api_exhausted"
			completed = now
		}
		total := max(locked.TotalReviewCount, page.TotalReviewCount)
		if locked.PagesFetched == math.MaxInt64 || locked.ReviewsFetched > math.MaxInt64-int64(len(page.Reviews)) {
			return googlereviews.ErrArchive
		}
		pages := locked.PagesFetched + 1
		reviewsFetched := locked.ReviewsFetched + int64(len(page.Reviews))
		_, err = tx.ExecContext(ctx, `
			UPDATE google_reviews_sync_state
			SET status = ?, next_page_token = ?, pages_fetched = ?, reviews_fetched = ?,
				total_review_count = ?, archived_review_count = ?, last_error_code = '',
				completed_at = ?, updated_at = ?
			WHERE target_name = ? AND location_name = ?`,
			status, page.NextPageToken, pages, reviewsFetched, total, archivedCount, completed, now, target, location)
		if err != nil {
			return fmt.Errorf("advance Google Reviews backfill checkpoint: %w", err)
		}
		state = googlereviews.BackfillState{
			Target: target, Location: location, Status: status, RunID: locked.RunID,
			PagesFetched: pages, ReviewsFetched: reviewsFetched, TotalReviewCount: total,
			ArchivedReviewCount: archivedCount, NextPageToken: page.NextPageToken,
			StartedAt: locked.StartedAt, UpdatedAt: timePointer(now),
		}
		if completed != nil {
			state.CompletedAt = timePointer(now)
		}
		return nil
	})
	if err != nil {
		return googlereviews.BackfillState{}, err
	}
	return state, nil
}

func (s *Store) FailGoogleReviewsBackfill(ctx context.Context, target, location, errorCode string) (googlereviews.BackfillState, error) {
	if !validGoogleReviewTarget(target, location) || !validGoogleReviewErrorCode(errorCode) {
		return googlereviews.BackfillState{}, googlereviews.ErrInvalid
	}
	now := time.Now().UTC().Format(googleReviewTimestampFormat)
	var state googlereviews.BackfillState
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE google_reviews_sync_state
			SET status = 'partial', last_error_code = ?, completed_at = NULL, updated_at = ?
			WHERE target_name = ? AND location_name = ? AND status = 'running'`, errorCode, now, target, location); err != nil {
			return fmt.Errorf("record Google Reviews backfill failure: %w", err)
		}
		var err error
		state, err = readGoogleReviewsBackfillState(ctx, tx, s.IsPostgreSQL(), target, location)
		if err != nil {
			return err
		}
		state.ArchivedReviewCount, err = countGoogleReviews(ctx, tx, target, location)
		return err
	})
	if err != nil {
		return googlereviews.BackfillState{}, err
	}
	return state, nil
}

func (s *Store) GoogleReviewsBackfillState(ctx context.Context, target string) (googlereviews.BackfillState, error) {
	if !validGoogleReviewID(target) {
		return googlereviews.BackfillState{}, googlereviews.ErrInvalid
	}
	query := `SELECT target_name, location_name, status, run_id, next_page_token, pages_fetched, reviews_fetched,
		total_review_count, archived_review_count, last_error_code, started_at, completed_at, updated_at
		FROM google_reviews_sync_state WHERE target_name = ? ORDER BY updated_at DESC, location_name DESC LIMIT 1`
	var state googlereviews.BackfillState
	var startedAt, updatedAt string
	var completedAt sql.NullString
	err := s.db.QueryRowContext(ctx, query, target).Scan(
		&state.Target, &state.Location, &state.Status, &state.RunID, &state.NextPageToken,
		&state.PagesFetched, &state.ReviewsFetched, &state.TotalReviewCount, &state.ArchivedReviewCount,
		&state.LastErrorCode, &startedAt, &completedAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return googlereviews.BackfillState{Target: target, Status: "not_started"}, nil
	}
	if err != nil {
		return googlereviews.BackfillState{}, fmt.Errorf("read Google Reviews backfill state: %w", err)
	}
	state.StartedAt = parseReviewTime(&startedAt)
	if completedAt.Valid {
		state.CompletedAt = parseReviewTime(&completedAt.String)
	}
	state.UpdatedAt = parseReviewTime(&updatedAt)
	state.ArchivedReviewCount, err = s.countGoogleReviewsContext(ctx, state.Target, state.Location)
	if err != nil {
		return googlereviews.BackfillState{}, err
	}
	return state, nil
}

func (s *Store) SearchGoogleReviews(ctx context.Context, query, target string, offset, limit int) (googlereviews.ArchiveSearchResponse, error) {
	if strings.TrimSpace(query) == "" || len(query) > 256 || offset < 0 || limit < 1 || limit > 100 || target != "" && !validGoogleReviewID(target) {
		return googlereviews.ArchiveSearchResponse{}, googlereviews.ErrSearch
	}
	parsed := search.Parse(query)
	ftsArg := s.dialect.BuildFTSArg(parsed.TextTerms)
	if ftsArg == "" {
		return googlereviews.ArchiveSearchResponse{}, googlereviews.ErrSearch
	}
	ftsPredicate := "r.id IN (SELECT rowid FROM google_reviews_fts WHERE google_reviews_fts MATCH ?)"
	if s.IsPostgreSQL() {
		ftsPredicate = `to_tsvector('simple', r.reviewer_display_name || ' ' || r.star_rating || ' ' || r.comment || ' ' || r.reply_comment) @@ to_tsquery('simple', ?)`
	}
	where := ftsPredicate
	args := []any{ftsArg}
	if target != "" {
		where += " AND r.target_name = ?"
		args = append(args, target)
	}
	var response googlereviews.ArchiveSearchResponse
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM google_reviews r WHERE "+where, args...).Scan(&response.Total); err != nil {
			return fmt.Errorf("count Google Reviews archive search results: %w", err)
		}
		pageArgs := append(append([]any(nil), args...), limit, offset)
		rows, err := tx.QueryContext(ctx, `
			SELECT r.target_name, r.location_name, r.review_id, r.reviewer_display_name,
				r.reviewer_profile_photo_url, r.reviewer_is_anonymous, r.star_rating, r.comment,
				r.create_time, r.update_time, r.has_reply, r.reply_comment, r.reply_update_time,
				r.reply_state, r.reply_policy_violation, r.archived_at
			FROM google_reviews r WHERE `+where+`
			ORDER BY COALESCE(NULLIF(r.update_time, ''), r.create_time) DESC, r.review_id
			LIMIT ? OFFSET ?`, pageArgs...)
		if err != nil {
			return fmt.Errorf("search Google Reviews archive: %w", err)
		}
		defer rows.Close()
		response.Results = make([]googlereviews.ArchivedReview, 0, limit)
		for rows.Next() {
			var targetName, location, reviewID, displayName, photoURL, rating, comment string
			var createTime, updateTime, replyComment, replyUpdateTime, replyState, violation, archivedAt string
			var anonymous, hasReply bool
			if err := rows.Scan(&targetName, &location, &reviewID, &displayName, &photoURL, &anonymous, &rating, &comment,
				&createTime, &updateTime, &hasReply, &replyComment, &replyUpdateTime, &replyState, &violation, &archivedAt); err != nil {
				return fmt.Errorf("scan Google Reviews archive search result: %w", err)
			}
			review := googlereviews.Review{
				Name: location + "/reviews/" + reviewID, ReviewID: reviewID,
				Reviewer:   googlereviews.Reviewer{DisplayName: displayName, ProfilePhotoURL: photoURL, IsAnonymous: anonymous},
				StarRating: rating, Comment: comment, CreateTime: createTime, UpdateTime: updateTime,
			}
			if hasReply {
				review.ReviewReply = &googlereviews.Reply{Comment: replyComment, UpdateTime: replyUpdateTime, ReviewReplyState: replyState, PolicyViolation: violation}
			}
			archivedTime := time.Time{}
			if parsed := parseReviewTime(&archivedAt); parsed != nil {
				archivedTime = *parsed
			}
			response.Results = append(response.Results, googlereviews.ArchivedReview{
				Target: targetName, Location: location, Review: review,
				ArchivedAt: archivedTime,
			})
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate Google Reviews archive search results: %w", err)
		}
		return nil
	})
	if err != nil {
		return googlereviews.ArchiveSearchResponse{}, err
	}
	response.Query = query
	response.Target = target
	response.Page = offset/limit + 1
	response.PageSize = limit
	return response, nil
}

func readGoogleReviewsBackfillState(ctx context.Context, tx *loggedTx, postgres bool, target, location string) (googlereviews.BackfillState, error) {
	query := `SELECT target_name, location_name, status, run_id, next_page_token, pages_fetched, reviews_fetched,
		total_review_count, archived_review_count, last_error_code, started_at, completed_at, updated_at
		FROM google_reviews_sync_state WHERE target_name = ? AND location_name = ?`
	if postgres {
		query += " FOR UPDATE"
	}
	var state googlereviews.BackfillState
	var startedAt, updatedAt string
	var completedAt sql.NullString
	err := tx.QueryRowContext(ctx, query, target, location).Scan(
		&state.Target, &state.Location, &state.Status, &state.RunID, &state.NextPageToken,
		&state.PagesFetched, &state.ReviewsFetched, &state.TotalReviewCount, &state.ArchivedReviewCount,
		&state.LastErrorCode, &startedAt, &completedAt, &updatedAt,
	)
	if err != nil {
		return googlereviews.BackfillState{}, fmt.Errorf("read Google Reviews backfill checkpoint: %w", err)
	}
	state.StartedAt = parseReviewTime(&startedAt)
	state.UpdatedAt = parseReviewTime(&updatedAt)
	if completedAt.Valid {
		state.CompletedAt = parseReviewTime(&completedAt.String)
	}
	return state, nil
}

func countGoogleReviews(ctx context.Context, tx *loggedTx, target, location string) (int64, error) {
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM google_reviews WHERE target_name = ? AND location_name = ?`, target, location).Scan(&count); err != nil {
		return 0, fmt.Errorf("count archived Google reviews: %w", err)
	}
	return count, nil
}

func (s *Store) countGoogleReviewsContext(ctx context.Context, target, location string) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM google_reviews WHERE target_name = ? AND location_name = ?`, target, location).Scan(&count); err != nil {
		return 0, fmt.Errorf("count archived Google reviews: %w", err)
	}
	return count, nil
}

func validGoogleReviewTarget(target, location string) bool {
	if !validGoogleReviewID(target) {
		return false
	}
	parts := strings.Split(location, "/")
	return len(parts) == 4 && parts[0] == "accounts" && validGoogleReviewID(parts[1]) &&
		parts[2] == "locations" && validGoogleReviewID(parts[3])
}

func validGoogleReviewID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func validGoogleReviewErrorCode(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func googleReviewTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func canonicalReviewTime(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(googleReviewTimestampFormat)
}

func parseReviewTime(value *string) *time.Time {
	if value == nil || *value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func timePointer(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func timeString(value *time.Time, fallback string) string {
	if value == nil {
		return fallback
	}
	return value.UTC().Format(time.RFC3339Nano)
}
