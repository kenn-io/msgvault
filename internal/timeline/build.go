package timeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
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
	if !req.Scope.Owner && len(sourcescope.IDs(ctx)) == 0 {
		return nil, ErrInvalidRequest
	}
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

	t := &Timeline{Mode: req.Mode, Timezone: loc.String(), TimezoneIsDefault: src.ZoneDefault, Now: now, Freshness: Freshness{AsOf: now, StaleThreshold: 86400, Sources: []SourceFreshness{}, Projections: []ProjectionFreshness{}, Warnings: []string{}}, Omitted: Omitted{Budget: map[string]any{}}}
	c := composer{ctx: ctx, src: src, req: req, t: t, loc: loc, positions: map[string]string{}, keys: map[string]string{}, entryDates: map[string]string{}, more: map[string]bool{}}
	windowAt := func(at time.Time) (Window, error) { return resolveWindow(req, at, loc) }
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
	if req.With != "" {
		p, e := c.resolvePerson()
		if e != nil {
			return nil, e
		}
		c.setSubject(p)
		if req.Mode == ModeRelationship {
			c.buildRelationship(p)
		}
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
	if req.Mode == ModeRelationship && req.With == "" || req.Mode == ModeAnchor && req.Around == "" {
		return req, ErrInvalidRequest
	}
	if req.Mode == ModeDay && req.With != "" {
		return req, ErrInvalidRequest
	}
	if req.Mode != ModeDay && req.Mode != ModeRelationship && req.Mode != ModeAnchor {
		return req, ErrInvalidRequest
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
	if req.Budget < 2048 || req.Budget > 65536 || req.Before < 0 || req.Before > 50 || req.After < 0 || req.After > 50 || req.Days < 0 {
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
		from = now.In(loc).AddDate(0, 0, 1-days).Format(time.DateOnly)
	}
	start, e1 := time.Parse(time.DateOnly, from)
	end, e2 := time.Parse(time.DateOnly, to)
	if e1 != nil || e2 != nil || end.Before(start) {
		return Window{}, ErrInvalidRequest
	}
	if !end.Before(start.AddDate(0, 0, 31)) {
		return Window{}, ErrWindowTooLarge
	}
	return Window{From: from, To: to}, nil
}

type composer struct {
	subject        store.ActivitySubject
	participants   []int64
	personResolved bool
	ctx            context.Context
	src            Sources
	req            Request
	t              *Timeline
	loc            *time.Location
	positions      map[string]string
	entryDates     map[string]string
	keys           map[string]string
	more           map[string]bool
	tiers          map[int64]store.PersonTier
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
			return compare(a.Section, b.Section)
		}
		return compare(a.Code, b.Code)
	})
	c.t.Gaps = slices.CompactFunc(c.t.Gaps, func(a, b Gap) bool {
		return a.Section == b.Section && a.Code == b.Code && slices.Equal(a.Items, b.Items)
	})
}
func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type readResult[T any] struct {
	value T
	err   error
}

// A buffered result lets a cancelled reader finish without holding up the page.
func readWithin[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) readResult[T] {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
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
func parallel(ctx context.Context, tasks ...func()) {
	if store.HasReadSnapshotContext(ctx) {
		for _, task := range tasks {
			task()
		}
		return
	}
	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Go(task)
	}
	wg.Wait()
}

func (c *composer) bounds() (time.Time, time.Time) {
	from, _, _ := calrecur.CivilDateBoundary(c.t.Window.From, c.loc)
	toDate := shiftCivilDate(c.t.Window.To, 1)
	to, _, _ := calrecur.CivilDateBoundary(toDate, c.loc)
	return from, to
}

func shiftCivilDate(date string, days int) string {
	civil, _ := time.Parse(time.DateOnly, date)
	return civil.AddDate(0, 0, days).Format(time.DateOnly)
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
	if c.src.People == nil || len(missing) == 0 {
		return
	}
	slices.Sort(missing)
	r := readWithin(c.ctx, requestTimeout, func(ctx context.Context) (map[int64]store.PersonTier, error) {
		return c.src.People.PersonTiersContext(ctx, missing, store.PersonTierOptions{Config: c.src.Filters.Exclusions, AsOf: c.t.Now})
	})
	c.gap("people", r.err, false)
	maps.Copy(c.tiers, r.value)
}
func (c *composer) tierPeople(people []Person) []Person {
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
func counterpartPeople(counterparts []store.ActivityMessageCounterpart) []Person {
	people := []Person{}
	seen := map[int64]bool{}
	for _, p := range counterparts {
		if p.Excluded != "" || seen[p.ParticipantID] {
			continue
		}
		seen[p.ParticipantID] = true
		name := p.DisplayName
		if name == "" {
			name = p.Address
		}
		people = append(people, Person{PersonID: p.PersonID, ParticipantID: p.ParticipantID, Name: name, TierSource: "none"})
	}
	return people
}

func (c *composer) collectLoops(personID int64) readResult[loops.Result] {
	limit := 10
	if c.req.Brief {
		limit = 5
	}
	return readWithin(c.ctx, loopsTimeout, func(ctx context.Context) (loops.Result, error) {
		return c.src.Loops.List(ctx, loops.Options{PersonID: personID, Limit: limit}, c.t.Now)
	})
}
