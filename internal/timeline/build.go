package timeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

const requestTimeout = 10 * time.Second
const loopsTimeout = 5 * time.Second
const maxBursts = 5000

// Build reads the authorized archive, composes a stable view and fits its final
// encoding. It never issues SQL or writes archive data.
func Build(ctx context.Context, src Sources, req Request) (*Timeline, error) {
	req, err := normalizeRequest(req)
	if err != nil {
		return nil, err
	}
	loc := src.Location
	if loc == nil {
		loc = time.UTC
	}
	clock := src.Clock
	if clock == nil {
		clock = time.Now
	}
	now := clock().UTC()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	t := &Timeline{Mode: req.Mode, Timezone: loc.String(), TimezoneIsDefault: src.ZoneDefault, Now: now, Freshness: Freshness{AsOf: now, Sources: []SourceFreshness{}, Projections: []ProjectionFreshness{}, Warnings: []string{}}, Omitted: Omitted{Budget: map[string]any{}}}
	c := composer{ctx: ctx, src: src, req: req, t: t, loc: loc, positions: map[string]string{}, keys: map[string]string{}, entryDates: map[string]string{}, more: map[string]bool{}}
	windowAt := func(at time.Time) (Window, error) { return resolveWindow(req, at, loc) }
	var person store.PersonCandidate
	if req.With != "" {
		p, e := c.resolvePerson()
		if e != nil {
			return nil, e
		}
		person = p
		c.setSubject(p)
	}
	var target anchorTarget
	if req.Mode == ModeAnchor {
		target, err = c.resolveAnchor()
		if err != nil {
			return nil, err
		}
		if req.Conversation && !target.at.IsZero() && target.conversationID <= 0 {
			return nil, ErrInvalidRequest
		}
		windowAt = func(at time.Time) (Window, error) {
			if target.at.IsZero() {
				return resolveWindow(req, at, loc)
			}
			return anchorWindow(target.at, loc), nil
		}
	}
	window, err := windowAt(now)
	if err != nil {
		return nil, err
	}
	t.Window = window
	binding := CursorBinding{ArchiveUID: src.ArchiveUID, Request: req, SourceIDs: sourcescope.IDs(ctx), Window: window, WindowAt: windowAt, Timezone: loc.String(), Now: now}
	positions := map[string]string{}
	if req.Cursor != "" {
		cursor, e := DecodeCursor(req.Cursor, binding)
		if e != nil {
			return nil, e
		}
		now, window, positions = cursor.Now, cursor.Window, cursor.Positions
		binding.Now, binding.Window = now, window
		t.Now, t.Window = now, window
		t.Freshness.AsOf = now
		c.positions = positions
	}
	if req.Mode == ModeRelationship {
		c.buildRelationship(person)
	}
	switch req.Mode {
	case ModeDay:
		c.buildDay()
	case ModeAnchor:
		c.buildAnchor(target)
	case ModeRelationship:
		// The person-specific view was composed after resolving the subject above.
	}
	c.finishGaps()
	SetContinuation(t, Continuation{Binding: binding, Positions: positions, More: c.more, SortKeys: c.keys})
	return Fit(t, req.Format, req.Budget)
}

func normalizeRequest(req Request) (Request, error) {
	if req.Mode == "" {
		req.Mode = ModeDay
		if req.With != "" {
			req.Mode = ModeRelationship
		}
		if req.Around != "" {
			req.Mode = ModeAnchor
		}
	}
	if req.Conversation && req.Around == "" {
		return req, fmt.Errorf("%w: conversation requires around", ErrInvalidRequest)
	}
	if req.Mode == ModeRelationship && req.With == "" || req.Mode == ModeAnchor && req.Around == "" {
		return req, ErrInvalidRequest
	}
	if req.Mode != ModeDay && req.Mode != ModeRelationship && req.Mode != ModeAnchor || req.Mode == ModeDay && req.With != "" {
		return req, ErrInvalidRequest
	}
	if req.Around != "" && req.Mode != ModeAnchor {
		return req, fmt.Errorf("%w: around conflicts with mode=%s", ErrInvalidRequest, req.Mode)
	}
	if req.Date != "" || req.Days != 0 || req.From != "" || req.To != "" {
		if req.Mode == ModeRelationship {
			return req, fmt.Errorf("%w: date/days/from/to conflict with with in relationship mode", ErrInvalidRequest)
		}
		if req.Mode == ModeAnchor {
			return req, fmt.Errorf("%w: date/days/from/to conflict with around", ErrInvalidRequest)
		}
	}
	if req.Around == "" && (req.BeforeSet || req.AfterSet || req.Before != 0 || req.After != 0) {
		return req, fmt.Errorf("%w: before/after require around", ErrInvalidRequest)
	}
	if req.Format == "" {
		req.Format = "markdown"
	}
	if req.Format != "markdown" && req.Format != "json" {
		return req, ErrInvalidRequest
	}
	if req.Budget == 0 {
		req.Budget = 16384
		if req.Brief {
			req.Budget = 6144
		}
	}
	if req.Brief && (req.Days != 0 || req.From != "" || req.To != "") {
		return req, fmt.Errorf("%w: brief conflicts with days/from/to", ErrInvalidRequest)
	}
	if req.Budget < 4096 || req.Budget > MaxBudget || req.Before < 0 || req.Before > 50 || req.After < 0 || req.After > 50 || req.Days < 0 {
		return req, ErrInvalidRequest
	}
	if req.Before == 0 && !req.BeforeSet {
		req.Before = 5
	}
	if req.After == 0 && !req.AfterSet {
		req.After = 5
	}
	for _, tier := range req.Tiers {
		if tier < 1 || tier > 3 {
			return req, ErrInvalidRequest
		}
	}
	if (req.Date != "" && (req.From != "" || req.To != "" || req.Days != 0)) || (req.Days != 0 && (req.From != "" || req.To != "")) || ((req.From == "") != (req.To == "")) {
		return req, ErrInvalidRequest
	}
	if req.Days > 31 {
		return req, ErrWindowTooLarge
	}
	return req, nil
}

func resolveWindow(req Request, now time.Time, loc *time.Location) (Window, error) {
	to := now.In(loc).Format(time.DateOnly)
	var from string
	if req.Date != "" {
		from, to = req.Date, req.Date
	} else if req.From != "" {
		from, to = req.From, req.To
	} else {
		days := req.Days
		if days == 0 {
			days = 2
		}
		if req.Brief {
			days = 2
		}
		from, _ = calrecur.AddCivilDateDays(to, 1-days)
	}
	start, e1 := time.Parse(time.DateOnly, from)
	end, e2 := time.Parse(time.DateOnly, to)
	if e1 != nil || e2 != nil || end.Before(start) {
		return Window{}, ErrInvalidRequest
	}
	for _, date := range []string{from, to} {
		if _, err := ParseAnchor(date, loc); err != nil {
			return Window{}, err
		}
	}
	if !end.Before(start.AddDate(0, 0, 31)) {
		return Window{}, ErrWindowTooLarge
	}
	return Window{From: from, To: to}, nil
}

type composer struct {
	identities   store.MeetingIdentityResult
	occurrences  map[string]store.CalendarOccurrence
	subject      store.ActivitySubject
	participants []int64
	ctx          context.Context
	src          Sources
	req          Request
	t            *Timeline
	loc          *time.Location
	positions    map[string]string
	entryDates   map[string]string
	keys         map[string]string
	more         map[string]bool
	tiers        map[int64]store.PersonTier
}

func (c *composer) gap(section string, err error, partial bool) {
	if err == nil {
		return
	}
	code := "unavailable"
	retry := false
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		code = "timeout"
		retry = true
	}
	if partial {
		code = "partial"
		retry = true
	}
	c.t.Gaps = append(c.t.Gaps, Gap{Section: section, Code: code, Retryable: retry})
}
func (c *composer) unavailable(section string) {
	c.t.Gaps = append(c.t.Gaps, Gap{Section: section, Code: "unavailable"})
}
func (c *composer) finishGaps() {
	slices.SortFunc(c.t.Gaps, func(a, b Gap) int {
		if a.Section != b.Section {
			return cmp.Compare(a.Section, b.Section)
		}
		return cmp.Compare(a.Code, b.Code)
	})
	c.t.Gaps = slices.CompactFunc(c.t.Gaps, func(a, b Gap) bool {
		return a.Section == b.Section && a.Code == b.Code && slices.Equal(a.Items, b.Items)
	})
}

type readResult[T any] struct {
	value T
	err   error
}

// A buffered result lets a cancelled reader finish without holding up the page.
func readWithin[T any](ctx context.Context, fn func(context.Context) (T, error)) readResult[T] {
	if store.HasReadSnapshotContext(ctx) {
		// Finish the reader before another task or snapshot release uses its
		// transaction. Store operations honor this context's deadline.
		value, err := fn(ctx)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return readResult[T]{value, err}
	}
	done := make(chan readResult[T], 1)
	go func() { value, err := fn(ctx); done <- readResult[T]{value, err} }()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		return readResult[T]{err: ctx.Err()}
	}
}
func (c *composer) parallel(tasks ...func()) {
	reads := make([]func(context.Context) error, 0, len(tasks))
	for _, task := range tasks {
		reads = append(reads, func(context.Context) error { task(); return nil })
	}
	_ = store.RunIndependentReads(c.ctx, c.src.db, reads...)
}

func (c *composer) bounds() (time.Time, time.Time) {
	from, _, _ := calrecur.CivilDateBoundary(c.t.Window.From, c.loc)
	end, _ := calrecur.AddCivilDateDays(c.t.Window.To, 1)
	to, _, _ := calrecur.CivilDateBoundary(end, c.loc)
	return from, to
}
func (c *composer) filters() store.ActivityFilters {
	f := c.src.Filters
	f.IncludeNoise = c.req.IncludeNoise
	return f
}
func (c *composer) open(tool string, args map[string]any) []Open {
	if c.req.Scope.Owner && c.req.Scope.OpenTools == nil || slices.Contains(c.req.Scope.OpenTools, tool) {
		return []Open{{Tool: tool, Args: args}}
	}
	return nil
}
func (c *composer) messageOpen(id int64) []Open {
	return c.open("get_message", map[string]any{"id": id})
}
func messageRef(id int64) string { return fmt.Sprintf("msg:%d", id) }
func (c *composer) admitTier(people []Person) bool {
	if len(c.req.Tiers) == 0 {
		return true
	}
	for _, p := range people {
		if p.PersonID > 0 && slices.Contains(c.req.Tiers, p.Tier) {
			return true
		}
	}
	return false
}

// loadTiers batches known IDs and caches missing/failed reads as well as hits.
// The cache lasts for one Build; source errors remain visible in people gaps.
func (c *composer) loadTiers(ids []int64) {
	if c.tiers == nil {
		c.tiers = map[int64]store.PersonTier{}
	}
	missing := []int64{}
	for _, id := range ids {
		if _, loaded := c.tiers[id]; id > 0 && !loaded {
			c.tiers[id] = store.PersonTier{}
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return
	}
	slices.Sort(missing)
	r := readWithin(c.ctx, func(ctx context.Context) (map[int64]store.PersonTier, error) {
		return c.src.People.PersonTiersContext(ctx, missing, store.PersonTierOptions{Config: c.src.Filters.Exclusions, AsOf: c.t.Now})
	})
	c.gap("people", r.err, false)
	maps.Copy(c.tiers, r.value)
}
func (c *composer) tierPeople(people []Person) []Person {
	people = uniquePeople(people)
	ids := make([]int64, 0, len(people))
	for _, p := range people {
		ids = append(ids, p.PersonID)
	}
	c.loadTiers(ids)
	for i, p := range people {
		people[i].TierSource = "none"
		if tier := c.tiers[p.PersonID]; p.PersonID > 0 && tier.Source != "" {
			people[i].Tier = tier.Tier
			people[i].TierSource = tier.Source
		}
	}
	return people
}

// participationOrder ranks call participation from weakest to strongest.
var participationOrder = []string{"invited", "joined"}

// uniquePeople merges people by identity, keeping the strongest participation and its credit.
func uniquePeople(people []Person) []Person {
	seen := map[string]int{}
	unique := people[:0]
	for _, p := range people {
		key := personKey(p)
		i, duplicate := seen[key]
		switch {
		case key == "":
			unique = append(unique, p)
		case !duplicate:
			seen[key] = len(unique)
			unique = append(unique, p)
		case slices.Index(participationOrder, p.Participation) > slices.Index(participationOrder, unique[i].Participation):
			unique[i].Participation, unique[i].Credited = p.Participation, p.Credited
		}
	}
	return unique
}

func personKey(p Person) string {
	switch {
	case p.PersonID > 0:
		return fmt.Sprintf("person:%d", p.PersonID)
	case p.ParticipantID > 0:
		return fmt.Sprintf("participant:%d", p.ParticipantID)
	case strings.TrimSpace(p.email) != "":
		return "email:" + strings.ToLower(strings.TrimSpace(p.email))
	}
	return ""
}

func counterpartPeople(counterparts []store.ActivityMessageCounterpart) []Person {
	people := []Person{}
	for _, p := range counterparts {
		if p.Excluded == "owner" {
			continue
		}
		name := p.DisplayName
		if name == "" {
			name = p.Address
		}
		people = append(people, Person{PersonID: p.PersonID, ParticipantID: p.ParticipantID, Name: name, email: p.Address, TierSource: "none"})
	}
	return uniquePeople(people)
}

func (c *composer) loopLimit() int {
	if c.req.Brief {
		return 5
	}
	return 10
}

// collectLoops reads loops past the visible limit when tiers filter them;
// addLoops applies the limit after resolving each counterparty's tier.
func (c *composer) collectLoops(personID int64) readResult[loops.Result] {
	limit := c.loopLimit()
	if len(c.req.Tiers) > 0 {
		limit = loops.MaxLimit
	}
	ctx, cancel := context.WithTimeout(c.ctx, loopsTimeout)
	defer cancel()
	return readWithin(ctx, func(ctx context.Context) (loops.Result, error) {
		return c.src.Loops.List(ctx, loops.Options{PersonID: personID, ParticipantIDs: c.subject.ParticipantIDs, Limit: limit}, c.t.Now)
	})
}
