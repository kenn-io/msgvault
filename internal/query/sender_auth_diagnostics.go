package query

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/senderauth"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

// AuthservObservation is an untrusted hostname found in an archived receipt.
// Its frequency is not evidence that the hostname is a safe trust pin.
type AuthservObservation struct {
	AuthservID   string `json:"authserv_id"`
	Count        int    `json:"count"`
	ContentTrust string `json:"content_trust"`
}

type SenderAuthDiagnostics struct {
	SourceID     int64                 `json:"source_id"`
	SampleLimit  int                   `json:"sample_limit"`
	Scanned      int                   `json:"scanned"`
	HasMore      bool                  `json:"has_more"`
	Verified     int                   `json:"verified"`
	BasisCounts  map[string]int        `json:"basis_counts"`
	Observations []AuthservObservation `json:"observations"`
}

// DiagnoseSenderAuth inspects a bounded newest-first sample in one source.
// It returns aggregates only, never message content or raw header blocks.
func (e *SQLiteEngine) DiagnoseSenderAuth(ctx context.Context, sourceID int64, limit int, suggest bool) (*SenderAuthDiagnostics, error) {
	if sourceID <= 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("source_id must be positive and limit must be between 1 and 1000")
	}
	ids, err := e.senderAuthSample(ctx, sourceID, limit)
	if err != nil {
		return nil, err
	}
	out := &SenderAuthDiagnostics{SourceID: sourceID, SampleLimit: limit, HasMore: len(ids) > limit, BasisCounts: map[string]int{}, Observations: []AuthservObservation{}}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out.Scanned = len(ids)
	verdicts := e.readSenderAuth(ctx, ids)
	for _, id := range ids {
		v := verdicts[id]
		out.BasisCounts[string(v.Basis)]++
		if v.Verified {
			out.Verified++
		}
	}
	if !suggest || len(ids) == 0 {
		return out, ctx.Err()
	}
	counts := map[string]int{}
	for start := 0; start < len(ids); start += messageSummaryIDChunk {
		batch := ids[start:min(start+messageSummaryIDChunk, len(ids))]
		placeholders := make([]string, len(batch))
		args := []any{senderAuthPrefixBytes}
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, sourceID)
		err := func() error {
			rows, err := e.queryContext(ctx, `SELECT substr(r.raw_data,1,?),COALESCE(r.compression,'') FROM message_raw r JOIN messages m ON m.id=r.message_id WHERE r.raw_format='mime' AND COALESCE(r.encryption_version,0)=0 AND m.id IN (`+strings.Join(placeholders, ",")+`) AND m.source_id=?`, args...)
			if err != nil {
				return fmt.Errorf("read receipt observations: %w", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var raw []byte
				var compression string
				if err = rows.Scan(&raw, &compression); err != nil {
					break
				}
				headers, basis := senderAuthHeaders(raw, compression)
				if basis != "" {
					continue
				}
				values := headers.Values("Authentication-Results")
				if len(values) == 0 {
					continue
				}
				observation := senderauth.Parse([]senderauth.Header{{Name: "Authentication-Results", Value: values[0]}}, nil, "", false)
				if len(observation.ObservedAuthservIDs) > 0 {
					counts[observation.ObservedAuthservIDs[0]]++
				}
			}
			rowErr := rows.Err()
			if err != nil {
				return err
			}
			if rowErr != nil {
				return rowErr
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	for id, count := range counts {
		out.Observations = append(out.Observations, AuthservObservation{AuthservID: id, Count: count, ContentTrust: "untrusted_text"})
	}
	slices.SortFunc(out.Observations, func(a, b AuthservObservation) int {
		if a.Count != b.Count {
			return cmp.Compare(b.Count, a.Count)
		}
		return strings.Compare(a.AuthservID, b.AuthservID)
	})
	return out, ctx.Err()
}

func (e *SQLiteEngine) senderAuthSample(ctx context.Context, sourceID int64, limit int) ([]int64, error) {
	conditions, args := appendSourceFilter([]string{"m.source_id=?", store.LiveMessagesWhere("m", false)}, []any{sourceID}, "m.", nil, sourcescope.IDs(ctx))
	args = append(args, limit+1)
	rows, err := e.queryContext(ctx, `SELECT m.id FROM messages m WHERE `+strings.Join(conditions, " AND ")+` ORDER BY `+e.dialect.DateOrderExpression("m.sent_at")+` DESC NULLS LAST,m.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("select sender-auth sample: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, limit+1)
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	rowErr := rows.Err()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return ids, nil
}
