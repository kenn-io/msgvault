package loops

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/sourceinventory"
	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrDisabled   = errors.New("open loops are disabled")
	ErrLoopsStale = errors.New("open loops archive may be stale")
)

type LoopsStaleResponse struct {
	Error     string           `json:"error"`
	Message   string           `json:"message"`
	Freshness freshness.Report `json:"freshness"`
}

type StaleError struct{ Response LoopsStaleResponse }

func (e *StaleError) Error() string { return e.Response.Message }
func (e *StaleError) Unwrap() error { return ErrLoopsStale }

type ListOptions struct {
	Kind, Status       string
	PersonID, SourceID int64
	IncludeSnoozed     bool
	Limit              int
	Cursor             string
}

type ListResponse struct {
	Items      []Loop           `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
	Freshness  freshness.Report `json:"freshness"`
}

type DetailResponse struct {
	Loop      Loop             `json:"loop"`
	Freshness freshness.Report `json:"freshness"`
}

type WaitingOptions struct {
	Top        int
	AllowStale bool
	AsOf       time.Time
}
type WaitingGroup struct {
	Counterparty      Counterparty `json:"counterparty"`
	LoopCount         int          `json:"loop_count"`
	OldestOwedSinceAt time.Time    `json:"oldest_owed_since_at"`
	Loops             []Loop       `json:"loops"`
}

type WaitingResponse struct {
	Groups    []WaitingGroup   `json:"groups"`
	Count     int              `json:"count"`
	Freshness freshness.Report `json:"freshness"`
}

// Service owns the same read and owner-action behavior for every transport.
// projection supplies certified applied progress from the deriver; nil leaves
// that projection explicitly missing. Loop settings are copied at construction.
// An attached API server can supply a synchronized live account snapshot for
// freshness inventory. Request clocks never alter persisted derivation state.
type Service struct {
	st              *store.Store
	cfg             config.Config
	accountSnapshot func() []config.AccountSchedule
	projection      func(context.Context) (freshness.ProjectionState, error)
	now             func() time.Time
}

func NewService(st *store.Store, cfg *config.Config, projection func(context.Context) (freshness.ProjectionState, error)) (*Service, error) {
	if st == nil || cfg == nil {
		return nil, errors.New("open loops: store and configuration are required")
	}
	if err := cfg.Loops.Validate(); err != nil {
		return nil, err
	}
	return &Service{st: st, cfg: *cfg, projection: projection, now: time.Now}, nil
}

// SetAccountSnapshot supplies the live configured accounts used by freshness
// reports. Install it before serving concurrent requests.
func (s *Service) SetAccountSnapshot(snapshot func() []config.AccountSchedule) {
	if s != nil {
		s.accountSnapshot = snapshot
	}
}

func (s *Service) Enabled() bool { return s != nil && s.cfg.Loops.Enabled }
func (s *Service) check() error {
	if !s.Enabled() {
		return ErrDisabled
	}
	return nil
}

// Freshness inventories all relevant configured sources, even without archive
// rows. Extra source IDs keep future loop kinds honest without widening the
// frozen email/chat sets. Static and disabled entries remain visible.
func (s *Service) Freshness(ctx context.Context, now time.Time, sourceIDs []int64) (freshness.Report, error) {
	if err := s.check(); err != nil {
		return freshness.Report{}, err
	}
	cfg := s.cfg
	if s.accountSnapshot != nil {
		cfg.Accounts = s.accountSnapshot()
	}
	items, err := sourceinventory.Build(ctx, &cfg, s.st)
	if err != nil {
		return freshness.Report{}, err
	}
	filtered := make([]freshness.SourceInventoryItem, 0, len(items))
	for _, item := range items {
		if slices.Contains(freshness.EmailSourceTypes, item.SourceType) || slices.Contains(freshness.ChatSourceTypes, item.SourceType) || (item.SourceID != nil && slices.Contains(sourceIDs, *item.SourceID)) {
			filtered = append(filtered, item)
		}
	}
	state := freshness.ProjectionState{}
	if s.projection != nil {
		state, err = s.projection(ctx)
		if err != nil {
			return freshness.Report{}, err
		}
	}
	return freshness.Build(ctx, freshness.Inputs{Now: now, StaleAfter: s.cfg.Loops.StaleAfter,
		Sources: filtered, Projections: map[string]freshness.ProjectionState{"open_loops": state}})
}

type listCursor struct {
	At time.Time `json:"at"`
	ID int64     `json:"id"`
}

func decodeListCursor(cursor string) (listCursor, error) {
	var c listCursor
	if len(cursor) > 512 {
		return c, fmt.Errorf("%w: invalid cursor", store.ErrInvalidLoop)
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return c, fmt.Errorf("%w: invalid cursor", store.ErrInvalidLoop)
	}
	if err := json.Unmarshal(data, &c); err != nil || c.At.IsZero() || c.ID <= 0 {
		return c, fmt.Errorf("%w: invalid cursor", store.ErrInvalidLoop)
	}
	return c, nil
}
func encodeListCursor(loop Loop) (string, error) {
	data, err := json.Marshal(struct {
		At string `json:"at"`
		ID int64  `json:"id"`
	}{At: loop.OwedSinceAt.Format(time.RFC3339Nano), ID: loop.ID})
	if err != nil {
		return "", fmt.Errorf("encode open-loop cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (s *Service) List(ctx context.Context, opts ListOptions) (ListResponse, error) {
	if err := s.check(); err != nil {
		return ListResponse{}, err
	}
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if opts.Status == "" {
		opts.Status = "open"
	}
	if opts.Limit < 1 || opts.Limit > 200 || opts.PersonID < 0 || opts.SourceID < 0 ||
		!slices.Contains([]string{"open", "held", "closed", StatusAll}, opts.Status) ||
		!slices.Contains([]string{"", ReplyOwed, AwaitingReply, "commitment_owed_by_me", "commitment_owed_to_me"}, opts.Kind) {
		return ListResponse{}, fmt.Errorf("%w: invalid list filters", store.ErrInvalidLoop)
	}
	now := s.now().UTC()
	filter := store.LoopListFilter{Kind: opts.Kind, Status: opts.Status, PersonID: opts.PersonID, SourceID: opts.SourceID,
		IncludeSnoozed: opts.IncludeSnoozed, Limit: opts.Limit + 1, Now: now}
	if opts.Cursor != "" {
		c, err := decodeListCursor(opts.Cursor)
		if err != nil {
			return ListResponse{}, err
		}
		filter.CursorAt, filter.CursorID, filter.HasCursor = c.At, c.ID, true
	}
	rows, err := s.st.ListOpenLoopsContext(ctx, filter)
	if err != nil {
		return ListResponse{}, err
	}
	more := len(rows) > opts.Limit
	if more {
		rows = rows[:opts.Limit]
	}
	result := ListResponse{Items: make([]Loop, 0, len(rows))}
	var sources []int64
	for _, row := range rows {
		loop, err := publicLoop(row, now)
		if err != nil {
			return ListResponse{}, err
		}
		result.Items = append(result.Items, loop)
		sources = append(sources, row.SourceID)
	}
	if more {
		result.NextCursor, err = encodeListCursor(result.Items[len(result.Items)-1])
		if err != nil {
			return ListResponse{}, err
		}
	}
	result.Freshness, err = s.Freshness(ctx, now, sources)
	return result, err
}

func (s *Service) Get(ctx context.Context, id int64) (DetailResponse, error) {
	if err := s.check(); err != nil {
		return DetailResponse{}, err
	}
	row, err := s.st.GetOpenLoopContext(ctx, id)
	if err != nil {
		return DetailResponse{}, err
	}
	now := s.now().UTC()
	loop, err := publicLoop(row, now)
	if err != nil {
		return DetailResponse{}, err
	}
	report, err := s.Freshness(ctx, now, []int64{row.SourceID})
	return DetailResponse{Loop: loop, Freshness: report}, err
}

// ListPersonOpenLoops implements the stable person-context Reader. Truncation
// comes from a sentinel row, while ComputedAt comes only from applied progress.
func (s *Service) ListPersonOpenLoops(ctx context.Context, personID int64, limit int) (Page, error) {
	if personID <= 0 {
		return Page{}, fmt.Errorf("%w: person must be positive", store.ErrInvalidLoop)
	}
	list, err := s.List(ctx, ListOptions{PersonID: personID, Limit: limit})
	if err != nil {
		return Page{}, err
	}
	page := Page{Loops: list.Items, Truncated: list.NextCursor != ""}
	if p := list.Freshness.Projections["open_loops"]; p != nil && p.ComputedAt != nil {
		page.ComputedAt = *p.ComputedAt
	}
	return page, nil
}

func publicLoop(row store.OpenLoop, now time.Time) (Loop, error) {
	var evidence []Evidence
	if err := json.Unmarshal(row.EvidenceJSON, &evidence); err != nil {
		return Loop{}, fmt.Errorf("decode loop %d evidence: %w", row.ID, err)
	}
	if evidence == nil {
		evidence = []Evidence{}
	}
	counterparty := Counterparty{PersonID: row.CounterpartyPersonID, DisplayName: row.CounterpartyDisplayName}
	if row.CounterpartyParticipantID != nil {
		counterparty.ParticipantID = *row.CounterpartyParticipantID
	}
	return Loop{ID: row.ID, TurnKey: row.TurnKey, Cycle: row.Cycle, Kind: row.Kind, Status: row.Status,
		Channel: row.Channel, ConversationID: row.ConversationID, SourceID: row.SourceID, Counterparty: counterparty,
		Summary: row.Summary, OwedSinceAt: row.OwedSinceAt, AnchorMessageID: row.AnchorMessageID,
		LastActivityMessageID: row.LastActivityMessageID, LastActivityAt: row.LastActivityAt, OpensAt: row.OpensAt,
		DueAt: row.DueAt, SnoozedUntil: row.SnoozedUntil, AgeSeconds: max(int64(0), int64(now.Sub(row.OwedSinceAt)/time.Second)),
		Evidence: evidence, ClosedAt: row.ClosedAt, ClosedReason: row.ClosedReason, ClosedMessageID: row.ClosedMessageID,
		Detector: row.Detector, AgendaState: row.AgendaState}, nil
}

var _ Reader = (*Service)(nil)
