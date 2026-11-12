package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/calrecur"
)

type meetingDecisionEdge struct {
	id         int64
	isExplicit bool
	a, b       meetingDecisionEndpoint
	decision   string
}

// meetingDecisionLoader reads the choices of every waiting message in one query, so a
// large group costs one lookup per batch instead of one per message.
type meetingDecisionLoader struct {
	store  *Store
	loaded map[meetingDecisionEndpoint][]meetingDecisionEdge
}

const meetingDecisionBatch = 400

// edges batches waiting endpoints while keeping recurring occurrences separate.
func (l *meetingDecisionLoader) edges(ctx context.Context, endpoint meetingDecisionEndpoint, waiting []meetingDecisionEndpoint, budget *int) ([]meetingDecisionEdge, error) {
	if edges, ok := l.loaded[endpoint]; ok {
		return edges, nil
	}
	endpoints := []meetingDecisionEndpoint{endpoint}
	for _, next := range waiting {
		_, done := l.loaded[next]
		if len(endpoints) < meetingDecisionBatch && !done && !slices.Contains(endpoints, next) {
			endpoints = append(endpoints, next)
		}
	}
	if l.loaded == nil {
		l.loaded = map[meetingDecisionEndpoint][]meetingDecisionEdge{}
	}
	if err := l.load(ctx, endpoints, budget); err != nil {
		return nil, err
	}
	return l.loaded[endpoint], nil
}

// meetingDecisionSideClause filters one stored endpoint before the read budget applies.
func meetingDecisionSideClause(endpoint meetingDecisionEndpoint, side string) (string, []any) {
	clause := "d." + side + "_message_id=?"
	args := []any{endpoint.ID}
	if endpoint.Kind != "" {
		clause += " AND d." + side + "_kind=?"
		args = append(args, endpoint.Kind)
		if endpoint.Kind == meetingRefEvent {
			clause += " AND (d." + side + "_key=? OR (d." + side + "_key='' AND d." + side + "_anchor=?))"
			args = append(args, endpoint.Key, endpoint.Key)
		}
	}
	return "(" + clause + ")", args
}

func meetingDecisionEndpointClause(endpoints []meetingDecisionEndpoint) (string, []any) {
	var clauses []string
	var args []any
	for _, endpoint := range endpoints {
		for _, side := range []string{"left", "right"} {
			clause, values := meetingDecisionSideClause(endpoint, side)
			clauses = append(clauses, clause)
			args = append(args, values...)
		}
	}
	return strings.Join(clauses, " OR "), args
}

func (l *meetingDecisionLoader) load(ctx context.Context, endpoints []meetingDecisionEndpoint, budget *int) error {
	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	where, args := meetingDecisionEndpointClause(endpoints)
	args = append(args, leftArgs...)
	args = append(args, rightArgs...)
	args = append(args, *budget+1)
	rows, err := l.store.db.QueryContext(ctx, `SELECT d.id,d.is_explicit,d.left_message_id,d.left_kind,d.left_key,d.left_anchor,d.right_message_id,d.right_kind,d.right_key,d.right_anchor,d.decision FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE (`+where+`) AND l.deleted_at IS NULL AND r.deleted_at IS NULL`+leftScope+rightScope+` LIMIT ?`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for _, endpoint := range endpoints {
		l.loaded[endpoint] = nil
	}
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.id, &edge.isExplicit, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.a.Anchor, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.b.Anchor, &edge.decision); err != nil {
			return err
		}
		for _, endpoint := range endpoints {
			if meetingDecisionEndpointMatches(endpoint, edge.a) || meetingDecisionEndpointMatches(endpoint, edge.b) {
				l.loaded[endpoint] = append(l.loaded[endpoint], edge)
			}
		}
		*budget--
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if *budget < 0 {
		return calendarRecurrenceBudgetError()
	}
	return nil
}

func meetingDecisionEndpointMatches(query, stored meetingDecisionEndpoint) bool {
	return query.ID == stored.ID && (query.Kind == "" || query.Kind == stored.Kind && (query.Kind != meetingRefEvent || query.Key == stored.Key || stored.Key == "" && stored.Anchor == query.Key))
}

// MeetingRecordContext follows automatic evidence and owner choices in one snapshot.
func (s *Store) MeetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	return s.meetingRecordContext(ctx, ref)
}

type meetingDecisionNode struct {
	endpoint meetingDecisionEndpoint
	link     *MeetingLink
}

type meetingDecisionWarning struct {
	endpoint meetingDecisionEndpoint
	warning  CalendarUnexpandable
}

type meetingDecisionGraph struct {
	nodes      map[string]meetingDecisionNode
	groups     [][]meetingDecisionEndpoint
	warnings   []meetingDecisionWarning
	candidates []meetingDecisionEdge
	automatic  MeetingRecord
}

func (s *Store) meetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	result := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	budget := calrecur.RequestReadBudget
	seed := endpointFromRef(ref)
	graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{seed}, &budget)
	if err != nil {
		return result, err
	}
	if len(graph.candidates) == 0 {
		return graph.automatic, nil
	}
	return graph.record(seed), nil
}

func (s *Store) loadMeetingDecisionGraph(ctx context.Context, seeds []meetingDecisionEndpoint, budget *int) (*meetingDecisionGraph, error) {
	nodes := map[string]meetingDecisionNode{}
	var queue []meetingDecisionEndpoint
	add := func(endpoint meetingDecisionEndpoint, link *MeetingLink) {
		key := endpoint.ref()
		if existing, ok := nodes[key]; ok {
			if existing.link == nil && link != nil {
				existing.link = link
				nodes[key] = existing
			}
			return
		}
		nodes[key] = meetingDecisionNode{endpoint, link}
		queue = append(queue, endpoint)
	}
	for _, seed := range seeds {
		add(seed, nil)
	}
	var warnings []meetingDecisionWarning
	var groups [][]meetingDecisionEndpoint
	var automatic MeetingRecord
	seenDecisions := map[int64]bool{}
	var candidates []meetingDecisionEdge
	resolved := map[string]bool{}
	materialized := map[string]MeetingRecord{}
	decisions := meetingDecisionLoader{store: s}
	for len(queue) > 0 {
		endpoint := queue[0]
		queue = queue[1:]
		if !resolved[endpoint.ref()] {
			record, cached := materialized[endpoint.ref()]
			var err error
			if !cached {
				record, err = s.meetingAutomaticRecordForEndpoint(ctx, endpoint, budget)
			}
			if errors.Is(err, sql.ErrNoRows) && endpoint != seeds[0] {
				continue
			}
			if err != nil {
				return nil, err
			}
			if endpoint == seeds[0] {
				automatic = record
			}
			for _, warning := range record.Unexpandable {
				warnings = append(warnings, meetingDecisionWarning{endpoint, warning})
			}
			members := []meetingDecisionEndpoint{endpoint}
			memberSet := map[string]bool{endpoint.ref(): true}
			addMember := func(member meetingDecisionEndpoint) {
				if !memberSet[member.ref()] {
					members = append(members, member)
					memberSet[member.ref()] = true
				}
			}
			for _, id := range record.MessageIDs {
				member := meetingDecisionEndpoint{ID: id, Kind: meetingRefMessage}
				add(member, nil)
				addMember(member)
			}
			for _, link := range record.Links {
				ref, err := ParseMeetingLinkRef(link.Ref)
				if err != nil {
					return nil, err
				}
				member := endpointFromRef(ref)
				linkCopy := link
				add(member, &linkCopy)
				addMember(member)
				if link.MessageID != member.ID {
					alias := meetingDecisionEndpoint{ID: link.MessageID, Kind: meetingRefEvent}
					add(alias, nil)
					addMember(alias)
				}
			}
			for _, member := range members {
				resolved[member.ref()] = true
			}
			groups = append(groups, members)
		}
		choices, err := decisions.edges(ctx, endpoint, queue, budget)
		if err != nil {
			return nil, err
		}
		for _, choice := range choices {
			if seenDecisions[choice.id] {
				continue
			}
			seenDecisions[choice.id] = true
			valid := true
			for _, endpoint := range []*meetingDecisionEndpoint{&choice.a, &choice.b} {
				canonical, _, record, err := s.materializedMeetingEndpointRecord(ctx, *endpoint, budget)
				if errors.Is(err, sql.ErrNoRows) {
					if len(record.Unexpandable) == 0 {
						valid = false
						break
					}
					canonical = *endpoint
					if canonical.Key == "" {
						canonical.Key = canonical.Anchor
					}
				} else if err != nil {
					return nil, err
				}
				materialized[canonical.ref()] = record
				*endpoint = canonical
			}
			if !valid {
				continue
			}
			add(choice.a, nil)
			add(choice.b, nil)
			candidates = append(candidates, choice)
		}
	}
	return &meetingDecisionGraph{nodes: nodes, groups: groups, warnings: warnings, candidates: candidates, automatic: automatic}, nil
}

func (g *meetingDecisionGraph) reduce() (func(string) string, []meetingDecisionEdge) {
	var explicit, splits []meetingDecisionEdge
	// Materialize aliases before choosing one effective contribution per pair.
	effective := map[string]meetingDecisionEdge{}
	for _, choice := range g.candidates {
		left, right := choice.a, choice.b
		if compareMeetingEndpoints(left, right) > 0 {
			left, right = right, left
		}
		choice.a, choice.b = left, right
		key := left.ref() + "/" + right.ref()
		old, exists := effective[key]
		if !exists || choice.isExplicit && !old.isExplicit || choice.isExplicit == old.isExplicit && choice.decision == "split" && old.decision != "split" {
			effective[key] = choice
		}
	}
	for _, choice := range effective {
		if choice.decision == "split" {
			splits = append(splits, choice)
		} else {
			explicit = append(explicit, choice)
		}
	}
	parent := map[string]string{}
	for key := range g.nodes {
		parent[key] = key
	}
	var root func(string) string
	root = func(key string) string {
		if parent[key] != key {
			parent[key] = root(parent[key])
		}
		return parent[key]
	}
	groupByMember := map[string][][]meetingDecisionEndpoint{}
	var vertices []meetingDecisionEndpoint
	for _, group := range g.groups {
		for _, member := range group {
			key := member.ref()
			if len(groupByMember[key]) == 0 {
				vertices = append(vertices, member)
			}
			groupByMember[key] = append(groupByMember[key], group)
		}
	}
	// A note/event split applies to all automatic event copies of that occurrence.
	for _, split := range slices.Clone(splits) {
		note, event := split.a, split.b
		if note.Kind == meetingRefEvent {
			note, event = event, note
		}
		if note.Kind != meetingRefMessage || event.Kind != meetingRefEvent {
			continue
		}
		for _, group := range groupByMember[event.ref()] {
			for _, member := range group {
				if member.Kind == meetingRefEvent && member.ref() != event.ref() {
					splits = append(splits, meetingDecisionEdge{a: note, b: member})
				}
			}
		}
	}
	slices.SortFunc(explicit, func(a, b meetingDecisionEdge) int {
		if c := compareMeetingEndpoints(a.a, b.a); c != 0 {
			return c
		}
		return compareMeetingEndpoints(a.b, b.b)
	})
	join := func(edge meetingDecisionEdge) {
		a, b := root(edge.a.ref()), root(edge.b.ref())
		if a == b {
			return
		}
		blocked := false
		for _, split := range splits {
			x, y := root(split.a.ref()), root(split.b.ref())
			if x == a && y == b || x == b && y == a {
				blocked = true
				break
			}
		}
		if !blocked {
			parent[b] = a
		}
	}
	for _, edge := range explicit {
		join(edge)
	}
	slices.SortFunc(vertices, compareMeetingEndpoints)
	for _, member := range vertices {
		var others []meetingDecisionEndpoint
		for _, group := range groupByMember[member.ref()] {
			for _, other := range group {
				if compareMeetingEndpoints(member, other) < 0 {
					others = append(others, other)
				}
			}
		}
		slices.SortFunc(others, compareMeetingEndpoints)
		for _, other := range others {
			join(meetingDecisionEdge{a: member, b: other})
		}
	}
	return root, explicit
}

func (g *meetingDecisionGraph) record(seed meetingDecisionEndpoint) MeetingRecord {
	result := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}, Unexpandable: []CalendarUnexpandable{}}
	root, _ := g.reduce()
	seedRoot := root(seed.ref())
	visibleEvents := map[int64]bool{}
	for key, n := range g.nodes {
		if n.endpoint.Kind == meetingRefEvent {
			visibleEvents[n.endpoint.ID] = visibleEvents[n.endpoint.ID] || root(key) == seedRoot
		}
	}
	// Keep a warning with its event when known, or with the group that found it.
	for _, entry := range g.warnings {
		visible, present := visibleEvents[entry.warning.MessageID]
		if visible || !present && root(entry.endpoint.ref()) == seedRoot {
			result.Unexpandable = append(result.Unexpandable, entry.warning)
		}
	}
	for key, n := range g.nodes {
		if root(key) != seedRoot {
			continue
		}
		if n.endpoint.Kind == meetingRefMessage {
			result.MessageIDs = append(result.MessageIDs, n.endpoint.ID)
		} else if n.link != nil {
			result.Links = append(result.Links, *n.link)
		}
	}
	slices.Sort(result.MessageIDs)
	result.MessageIDs = slices.Compact(result.MessageIDs)
	slices.SortFunc(result.Links, func(a, b MeetingLink) int {
		if a.Start == nil && b.Start != nil {
			return 1
		}
		if a.Start != nil && b.Start == nil {
			return -1
		}
		if a.Start != nil && b.Start != nil {
			if c := a.Start.Compare(*b.Start); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(a.MessageID, b.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Ref, b.Ref)
	})
	slices.SortFunc(result.Unexpandable, func(a, b CalendarUnexpandable) int {
		if c := cmp.Compare(a.MessageID, b.MessageID); c != 0 {
			return c
		}
		return cmp.Compare(a.Reason, b.Reason)
	})
	result.Unexpandable = slices.Compact(result.Unexpandable)
	return result
}
