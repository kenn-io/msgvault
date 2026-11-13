package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/calrecur"
)

type meetingDecisionEdge struct {
	id       int64
	a, b     meetingDecisionEndpoint
	decision string
}

func newerMeetingDecision(a, b meetingDecisionEdge) bool {
	return a.id > b.id
}

// meetingDecisionEdges finds choices saved on the occurrence or its hidden copies.
func (s *Store) meetingDecisionEdges(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) ([]meetingDecisionEdge, error) {
	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	var originalDate string
	var originalTime any
	if endpoint.Kind == meetingRefEvent && endpoint.Key != "" {
		ref, err := endpoint.meetingRef()
		if err != nil {
			return nil, err
		}
		if ref.Identity.AllDay {
			originalDate = ref.Identity.Date
		} else {
			originalTime = ref.Identity.Instant
		}
	}
	args := []any{originalDate, originalTime, endpoint.ID, endpoint.Kind, endpoint.Key, endpoint.Key, endpoint.Kind, endpoint.Key, endpoint.Key}
	args = append(args, leftArgs...)
	args = append(args, rightArgs...)
	args = append(args, *budget+1)
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE edges(child, parent) AS (
 SELECT c.message_id,c.survivor_message_id FROM meeting_copy_survivors c JOIN messages m ON m.id=c.message_id WHERE m.deleted_at IS NOT NULL AND m.delete_batch_id IS NOT NULL
 UNION SELECT c.message_id,c.survivor_message_id FROM source_merge_messages c JOIN messages m ON m.id=c.message_id WHERE m.deleted_at IS NOT NULL AND (m.delete_batch_id IS NULL OR NOT EXISTS (SELECT 1 FROM meeting_copy_survivors x WHERE x.message_id=m.id)) AND c.survivor_message_id IS NOT NULL
 UNION SELECT e.message_id,series.message_id FROM calendar_events e JOIN messages m ON m.id=e.message_id JOIN calendar_events series ON series.provider_event_id=e.series_provider_id JOIN messages sm ON sm.id=series.message_id AND sm.source_id=m.source_id WHERE e.kind='exception' AND e.series_provider_id<>'' AND series.kind='series' AND m.deleted_at IS NULL AND ((e.original_start_date<>'' AND e.original_start_date=?) OR e.original_start_at=?)
 ), ids(id) AS (SELECT CAST(? AS BIGINT) UNION SELECT edges.child FROM edges JOIN ids ON edges.parent=ids.id
 ) SELECT d.id,d.left_message_id,d.left_kind,d.left_key,d.right_message_id,d.right_kind,d.right_key,d.decision FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE ((d.left_message_id IN (SELECT id FROM ids) AND (d.left_kind=? AND (d.left_key=? OR d.left_key='' OR ?=''))) OR (d.right_message_id IN (SELECT id FROM ids) AND (d.right_kind=? AND (d.right_key=? OR d.right_key='' OR ?=''))))`+leftScope+rightScope+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var edges []meetingDecisionEdge
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.id, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.decision); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
		*budget--
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if *budget < 0 {
		return nil, calendarRecurrenceBudgetError()
	}
	return edges, nil
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
	seed, err := s.meetingOccurrenceKey(ctx, endpointFromRef(ref), &budget)
	if err != nil {
		return result, err
	}
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
	for len(queue) > 0 {
		endpoint := queue[0]
		queue = queue[1:]
		if !resolved[endpoint.ref()] {
			record, err := s.meetingAutomaticRecordForEndpoint(ctx, endpoint, budget)
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
				member, err := s.meetingOccurrenceKey(ctx, endpointFromRef(ref), budget)
				if err != nil {
					return nil, err
				}
				linkCopy := link
				add(member, &linkCopy)
				addMember(member)
			}
			for _, member := range members {
				resolved[member.ref()] = true
			}
			groups = append(groups, members)
		}
		choices, err := s.meetingDecisionEdges(ctx, endpoint, budget)
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
				canonical, err := s.meetingOccurrenceKey(ctx, *endpoint, budget)
				if errors.Is(err, ErrMeetingOccurrenceRequired) || errors.Is(err, sql.ErrNoRows) {
					valid = false
					break
				}
				if err != nil {
					return nil, err
				}
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

func (g *meetingDecisionGraph) reduce() (func(string) string, []meetingDecisionEdge, map[string]string) {
	var explicit, splits []meetingDecisionEdge
	// Choose one effective contribution per pair.
	effective := map[string]meetingDecisionEdge{}
	for _, choice := range g.candidates {
		left, right := choice.a, choice.b
		if compareMeetingEndpoints(left, right) > 0 {
			left, right = right, left
		}
		choice.a, choice.b = left, right
		key := left.ref() + "/" + right.ref()
		old, exists := effective[key]
		if !exists || newerMeetingDecision(choice, old) {
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
		for _, split := range splits {
			x, y := root(split.a.ref()), root(split.b.ref())
			if x == a && y == b || x == b && y == a {
				return
			}
		}
		parent[b] = a
	}
	slices.SortFunc(vertices, compareMeetingEndpoints)
	// Join copies first so a note split applies to every copy still joined.
	var others []meetingDecisionEndpoint
	copyRoots := map[string]string{}
	for _, events := range []bool{true, false} {
		for _, pair := range explicit {
			if (pair.a.Kind == meetingRefEvent && pair.b.Kind == meetingRefEvent) == events {
				join(pair)
			}
		}
		for _, member := range vertices {
			others = others[:0]
			for _, group := range groupByMember[member.ref()] {
				for _, other := range group {
					if compareMeetingEndpoints(member, other) < 0 {
						others = append(others, other)
					}
				}
			}
			slices.SortFunc(others, compareMeetingEndpoints)
			for _, other := range others {
				if (member.Kind == meetingRefEvent && other.Kind == meetingRefEvent) == events {
					join(meetingDecisionEdge{a: member, b: other})
				}
			}
		}
		if events {
			for key, node := range g.nodes {
				if node.endpoint.Kind == meetingRefEvent {
					copyRoots[key] = root(key)
				}
			}
		}
	}
	return root, explicit, copyRoots
}

func (g *meetingDecisionGraph) record(seed meetingDecisionEndpoint) MeetingRecord {
	result := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}, Unexpandable: []CalendarUnexpandable{}}
	root, _, _ := g.reduce()
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
