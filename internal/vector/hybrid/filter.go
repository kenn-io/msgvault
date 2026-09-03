package hybrid

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
)

// BuildFilter translates a parsed Gmail-syntax query into a
// vector.Filter by resolving address/label tokens to IDs against the
// main DB. Matches the semantics of the existing SQLite search path
// (internal/store/api.go): address operators use substring LIKE
// against participants.email_address; labels are matched by exact
// name; subject terms, size bounds, attachment and date filters pass
// through unchanged.
//
// Repeated same-field operators (e.g. `from:alice from:bob`,
// `to:alice to:bob`, `label:promo label:billing`) preserve per-token
// AND semantics at the message level: each token resolves to a group
// of participant/label IDs, and the backend emits one EXISTS clause
// per group, AND'd together. A single message with two `from`
// recipients (one alice, one bob) satisfies both tokens.
//
// Caller is responsible for any additional filter fields that do not
// derive from the query string (e.g. a SourceID coming from an HTTP
// account parameter) — just set them on the returned Filter.
//
// rebind converts the ? placeholders in the participant/label lookup
// SQL to the driver's native form. Pass the dialect's Rebind on
// PostgreSQL (so ? becomes $N — pgx rejects bare ?); pass nil (or
// SQLiteDialect.Rebind, which is identity) on SQLite to leave the
// queries unchanged.
func BuildFilter(ctx context.Context, db *sql.DB, rebind func(string) string, q *search.Query) (vector.Filter, error) {
	var f vector.Filter
	if q == nil {
		return f, nil
	}
	if rebind == nil {
		rebind = identityRebind
	}

	groupFilters := []struct {
		addrs []string
		dst   *[][]int64
	}{
		{q.FromAddrs, &f.SenderGroups},
		{q.ToAddrs, &f.ToGroups},
		{q.CcAddrs, &f.CcGroups},
		{q.BccAddrs, &f.BccGroups},
	}
	for _, gf := range groupFilters {
		if len(gf.addrs) == 0 {
			continue
		}
		groups, err := resolveAddressGroups(ctx, db, rebind, gf.addrs)
		if err != nil {
			return f, err
		}
		*gf.dst = groups
	}

	if len(q.Labels) > 0 {
		groups, err := resolveLabelGroups(ctx, db, rebind, q.Labels)
		if err != nil {
			return f, err
		}
		f.LabelGroups = groups
	}

	if q.HasAttachment != nil {
		v := *q.HasAttachment
		f.HasAttachment = &v
	}
	if q.AfterDate != nil {
		f.After = q.AfterDate
	}
	if q.BeforeDate != nil {
		f.Before = q.BeforeDate
	}
	if q.LargerThan != nil {
		v := *q.LargerThan
		f.LargerThan = &v
	}
	if q.SmallerThan != nil {
		v := *q.SmallerThan
		f.SmallerThan = &v
	}
	if len(q.SubjectTerms) > 0 {
		f.SubjectSubstrings = append([]string(nil), q.SubjectTerms...)
	}
	if len(q.ListIDs) > 0 {
		f.ListIDSubstrings = append([]string(nil), q.ListIDs...)
	}
	if len(q.ListIDExactGroups) > 0 {
		f.ListIDExactGroups = make([][]string, len(q.ListIDExactGroups))
		for i, group := range q.ListIDExactGroups {
			f.ListIDExactGroups[i] = append([]string(nil), group...)
		}
	}
	if len(q.MessageTypes) > 0 {
		f.MessageTypes = append([]string(nil), q.MessageTypes...)
	}
	if len(q.AccountIDs) > 0 {
		f.SourceIDs = append([]int64(nil), q.AccountIDs...)
	}
	if q.ConversationIDs != nil {
		if len(q.ConversationIDs) == 0 {
			f.ConversationIDs = []int64{noMatchSentinel}
		} else {
			f.ConversationIDs = append([]int64(nil), q.ConversationIDs...)
		}
	}
	return f, nil
}

// ApplyMessageFilter intersects an exact structured TUI filter with the
// Gmail-syntax filter already built from the user's search query. Address,
// domain, and label drill values are resolved with equality semantics; this
// deliberately differs from user-authored from:/label: operators, which keep
// their documented substring behavior.
func ApplyMessageFilter(
	ctx context.Context,
	db *sql.DB,
	rebind func(string) string,
	f *vector.Filter,
	structured query.MessageFilter,
) error {
	if rebind == nil {
		rebind = identityRebind
	}
	if period := structured.TimeRange.Period; period != "" {
		if _, _, ok := query.ParseTimePeriodBounds(period); !ok {
			return fmt.Errorf("invalid time_period %q: expected YYYY, YYYY-MM, or YYYY-MM-DD", period)
		}
	}

	derived := query.MergeFilterIntoQuery(&search.Query{}, structured)
	intersectSourceIDs(f, derived.AccountIDs)
	intersectConversationIDs(f, derived.ConversationIDs)
	if derived.AfterDate != nil {
		if f.After == nil || derived.AfterDate.After(*f.After) {
			f.After = derived.AfterDate
		}
	}
	if derived.BeforeDate != nil {
		if f.Before == nil || derived.BeforeDate.Before(*f.Before) {
			f.Before = derived.BeforeDate
		}
	}
	if derived.HasAttachment != nil && *derived.HasAttachment {
		value := true
		f.HasAttachment = &value
	}
	if len(derived.MessageTypes) > 0 {
		f.MessageTypes = intersectMessageTypes(f.MessageTypes, derived.MessageTypes)
	}
	if structured.ListID != "" {
		f.ListID = structured.ListID
	}

	if structured.Sender != "" {
		group, err := resolveExactParticipantIDs(ctx, db, rebind,
			"email_address = ? OR phone_number = ?", structured.Sender, structured.Sender)
		if err != nil {
			return err
		}
		f.SenderExactGroups = append(f.SenderExactGroups, noMatchGroup(group))
	}
	if structured.Recipient != "" {
		group, err := resolveExactParticipantIDs(ctx, db, rebind,
			"email_address = ?", structured.Recipient)
		if err != nil {
			return err
		}
		f.RecipientAnyGroups = append(f.RecipientAnyGroups, noMatchGroup(group))
	}
	if structured.Domain != "" {
		group, err := resolveExactParticipantIDs(ctx, db, rebind, "domain = ?", structured.Domain)
		if err != nil {
			return err
		}
		// Domain drill-downs intentionally match only explicit `from`
		// recipient rows, matching query.MessageFilter semantics.
		f.SenderGroups = append(f.SenderGroups, noMatchGroup(group))
	}
	if structured.Label != "" {
		group, err := resolveExactLabelIDs(ctx, db, rebind, structured.Label)
		if err != nil {
			return err
		}
		f.LabelGroups = append(f.LabelGroups, noMatchGroup(group))
	}
	return nil
}

func intersectSourceIDs(f *vector.Filter, exact []int64) {
	if exact == nil {
		return
	}
	if len(f.SourceIDs) == 0 {
		f.SourceIDs = append([]int64(nil), exact...)
		return
	}
	allowed := make(map[int64]struct{}, len(exact))
	for _, id := range exact {
		allowed[id] = struct{}{}
	}
	out := f.SourceIDs[:0]
	for _, id := range f.SourceIDs {
		if _, ok := allowed[id]; ok {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		out = []int64{noMatchSentinel}
	}
	f.SourceIDs = out
}

func intersectConversationIDs(f *vector.Filter, exact []int64) {
	if exact == nil {
		return
	}
	if len(f.ConversationIDs) == 0 {
		f.ConversationIDs = append([]int64(nil), exact...)
		return
	}
	allowed := make(map[int64]struct{}, len(exact))
	for _, id := range exact {
		allowed[id] = struct{}{}
	}
	out := f.ConversationIDs[:0]
	for _, id := range f.ConversationIDs {
		if _, ok := allowed[id]; ok {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		out = []int64{noMatchSentinel}
	}
	f.ConversationIDs = out
}

func intersectMessageTypes(existing, exact []string) []string {
	if len(existing) == 0 {
		return append([]string(nil), exact...)
	}
	allowed := make(map[string]struct{}, len(exact))
	for _, value := range exact {
		allowed[strings.ToLower(value)] = struct{}{}
	}
	var out []string
	for _, value := range existing {
		if _, ok := allowed[strings.ToLower(value)]; ok {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return []string{"__msgvault_no_matching_message_type__"}
	}
	return out
}

func resolveExactParticipantIDs(
	ctx context.Context,
	db *sql.DB,
	rebind func(string) string,
	where string,
	args ...any,
) ([]int64, error) {
	rows, err := db.QueryContext(ctx, rebind("SELECT id FROM participants WHERE "+where), args...)
	if err != nil {
		return nil, fmt.Errorf("query exact participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan exact participant id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate exact participants: %w", err)
	}
	return ids, nil
}

func resolveExactLabelIDs(ctx context.Context, db *sql.DB, rebind func(string) string, label string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, rebind("SELECT id FROM labels WHERE LOWER(name) = LOWER(?)"), label)
	if err != nil {
		return nil, fmt.Errorf("query exact labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan exact label id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate exact labels: %w", err)
	}
	return ids, nil
}

func noMatchGroup(ids []int64) []int64 {
	if len(ids) == 0 {
		return []int64{noMatchSentinel}
	}
	return ids
}

// resolveAddressGroups produces one IDs slice per supplied token. The
// backend AND-combines groups: a message must have at least one
// recipient matching every group. An individual token that resolves
// to zero participants collapses to the no-match sentinel for that
// group, which makes the per-group EXISTS check fail and returns zero
// hits overall — preserving the SQLite path's "any unknown token
// poisons the whole field" semantic.
func resolveAddressGroups(ctx context.Context, db *sql.DB, rebind func(string) string, addrs []string) ([][]int64, error) {
	groups := make([][]int64, 0, len(addrs))
	for _, a := range addrs {
		ids, err := resolveParticipantIDs(ctx, db, rebind, []string{a})
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			ids = []int64{noMatchSentinel}
		}
		groups = append(groups, ids)
	}
	return groups, nil
}

// resolveLabelGroups is the label-side counterpart of
// resolveAddressGroups.
func resolveLabelGroups(ctx context.Context, db *sql.DB, rebind func(string) string, labels []string) ([][]int64, error) {
	groups := make([][]int64, 0, len(labels))
	for _, l := range labels {
		ids, err := resolveLabelIDs(ctx, db, rebind, []string{l})
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			ids = []int64{noMatchSentinel}
		}
		groups = append(groups, ids)
	}
	return groups, nil
}

// resolveParticipantIDs returns every participant whose email_address
// contains any of the supplied tokens as a substring. Mirrors the
// `from:` / `to:` behavior in internal/store/api.go so vector/hybrid
// search agrees with the FTS path.
func resolveParticipantIDs(ctx context.Context, db *sql.DB, rebind func(string) string, addrs []string) ([]int64, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(addrs))
	args := make([]any, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, `LOWER(email_address) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(strings.ToLower(a))+"%")
	}
	q := rebind("SELECT id FROM participants WHERE " + strings.Join(parts, " OR "))
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan participant id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate participants: %w", err)
	}
	return ids, nil
}

// resolveLabelIDs returns labels whose name contains any of the
// supplied tokens as a case-insensitive substring. Mirrors the
// `label:` behavior in internal/store/api.go (LOWER(l.name) LIKE
// '%token%' ESCAPE '\') so vector/hybrid search agrees with the FTS
// path on which label matches a user-supplied token.
func resolveLabelIDs(ctx context.Context, db *sql.DB, rebind func(string) string, labels []string) ([]int64, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(labels))
	args := make([]any, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, `LOWER(name) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(strings.ToLower(l))+"%")
	}
	q := rebind("SELECT id FROM labels WHERE " + strings.Join(parts, " OR "))
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan label id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate labels: %w", err)
	}
	return ids, nil
}

// noMatchSentinel is the id stored in a resolved-to-empty filter
// slice. SQLite auto-increment ids start at 1, so -1 is guaranteed
// not to match any real row. BuildFilter substitutes this when a
// requested operator resolves to zero participants/labels, so the
// backend IN (...) check returns zero rows instead of degrading
// back to "unrestricted".
const noMatchSentinel int64 = -1

// identityRebind leaves a query unchanged. Used as the SQLite default
// when BuildFilter is called with a nil rebind (SQLite's ? placeholders
// are already native, so no rewrite is needed).
func identityRebind(q string) string { return q }

// escapeLike escapes SQL LIKE special characters (%, _, \) so they
// are matched literally. Used with ESCAPE '\'. Mirrors escapeLike in
// internal/store/api.go.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
