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
	id       int64
	a, b     meetingDecisionEndpoint
	decision string
}

func newerMeetingDecision(a, b meetingDecisionEdge) bool {
	return a.id > b.id
}

// meetingDecisionEdges finds choices saved on the occurrence or its exception.
func (s *Store) meetingDecisionEdges(ctx context.Context, endpoint meetingDecisionEndpoint, budget *int) ([]meetingDecisionEdge, error) {
	groups, err := s.meetingDecisionEdgesBatch(ctx, []meetingDecisionEndpoint{endpoint}, budget)
	return groups[endpoint], err
}

// meetingDecisionEdgesBatch shares copy traversal among occurrences with the same key.
func (s *Store) meetingDecisionEdgesBatch(ctx context.Context, endpoints []meetingDecisionEndpoint, budget *int) (map[meetingDecisionEndpoint][]meetingDecisionEdge, error) {
	type key struct{ kind, occurrence string }
	groups := map[key][]meetingDecisionEndpoint{}
	var keys []key
	for _, endpoint := range endpoints {
		k := key{endpoint.Kind, endpoint.Key}
		if len(groups[k]) == 0 {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], endpoint)
	}
	result := map[meetingDecisionEndpoint][]meetingDecisionEdge{}
	reader := s.meetingSQLReader(ctx)
	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	copyScope, copyArgs := sourceScopeClause(ctx, "m.source_id")
	parentScope, parentArgs := sourceScopeClause(ctx, "parent.source_id")
	editScope, editArgs := sourceScopeClause(ctx, "em.source_id")
	seriesScope, seriesArgs := sourceScopeClause(ctx, "sm.source_id")
	for _, k := range keys {
		args := []any{k.kind, k.occurrence, k.kind, k.occurrence}
		args = append(args, leftArgs...)
		args = append(args, rightArgs...)
		var hasChoices bool
		err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE ((d.left_kind=? AND d.left_key=?) OR (d.right_kind=? AND d.right_key=?))`+leftScope+rightScope+`)`, args...).Scan(&hasChoices)
		if err != nil {
			return nil, err
		}
		if !hasChoices {
			continue
		}
		var originalDate string
		var originalTime any
		if k.kind == meetingRefEvent && k.occurrence != "" {
			ref, err := groups[k][0].meetingRef()
			if err != nil {
				return nil, err
			}
			if ref.Identity.AllDay {
				originalDate = ref.Identity.Date
			} else {
				originalTime = ref.Identity.Instant
			}
		}
		for batch := range slices.Chunk(groups[k], 100) {
			values := make([]string, len(batch))
			args := make([]any, 0, len(batch)*2+9)
			for i, endpoint := range batch {
				values[i] = "(CAST(? AS INTEGER), CAST(? AS BIGINT))"
				args = append(args, i, endpoint.ID)
			}
			args = append(args, copyArgs...)
			args = append(args, parentArgs...)
			args = append(args, originalDate, originalTime)
			args = append(args, editArgs...)
			args = append(args, seriesArgs...)
			args = append(args, k.kind, k.occurrence, k.kind, k.occurrence)
			args = append(args, leftArgs...)
			args = append(args, rightArgs...)
			args = append(args, *budget+1)
			series := meetingSeriesLinksSQL(` AND ((e.original_start_date<>'' AND e.original_start_date=?) OR e.original_start_at=?)` + editScope + seriesScope)
			query := `WITH RECURSIVE inputs(idx,id) AS (VALUES ` + strings.Join(values, ",") + `), ` + meetingCopyGraphSQL(`SELECT i.idx,m.id FROM inputs i JOIN messages m ON m.id=i.id`, copyScope, parentScope, series) + `
 SELECT i.idx,d.id,d.left_message_id,d.left_kind,d.left_key,d.right_message_id,d.right_kind,d.right_key,d.decision FROM inputs i JOIN meeting_link_decisions d ON ((d.left_message_id IN (SELECT id FROM copies WHERE idx=i.idx) AND d.left_kind=? AND d.left_key=?) OR (d.right_message_id IN (SELECT id FROM copies WHERE idx=i.idx) AND d.right_kind=? AND d.right_key=?)) JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE TRUE` + leftScope + rightScope + ` LIMIT ?`
			err := func() error {
				rows, err := reader.QueryContext(ctx, query, args...)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var index int
					var edge meetingDecisionEdge
					if err := rows.Scan(&index, &edge.id, &edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.decision); err != nil {
						return err
					}
					result[batch[index]] = append(result[batch[index]], edge)
					*budget--
				}
				return errors.Join(rows.Err(), rows.Close())
			}()
			if err != nil {
				return nil, err
			}
			if *budget < 0 {
				return nil, calendarRecurrenceBudgetError()
			}
		}
	}
	return result, nil
}

// MeetingRecordContext follows automatic evidence and owner choices in one snapshot.
func (s *Store) MeetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	return s.meetingRecordContext(ctx, ref, false)
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

func (s *Store) meetingRecordContext(ctx context.Context, ref MeetingLinkRef, notesOnly bool) (MeetingRecord, error) {
	result := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	budget := calrecur.RequestReadBudget
	if err := s.validateMeetingInput(ctx, endpointFromRef(ref), &budget); err != nil {
		return result, err
	}
	seed, err := s.meetingOccurrenceKey(ctx, endpointFromRef(ref), &budget)
	if err != nil {
		return result, err
	}
	graph, err := s.loadMeetingDecisionGraph(ctx, []meetingDecisionEndpoint{seed}, &budget, notesOnly)
	if err != nil {
		return result, err
	}
	if len(graph.candidates) == 0 {
		return graph.automatic, nil
	}
	return graph.record(seed), nil
}

func (s *Store) loadMeetingDecisionGraph(ctx context.Context, seeds []meetingDecisionEndpoint, budget *int, notesOnly bool) (*meetingDecisionGraph, error) {
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
		var pending []meetingDecisionEndpoint
		for len(queue) > 0 {
			endpoint := queue[0]
			queue = queue[1:]
			if !resolved[endpoint.ref()] {
				record, err := s.meetingAutomaticRecordForEndpoint(ctx, endpoint, budget, notesOnly)
				if errors.Is(err, sql.ErrNoRows) && endpoint != seeds[0] {
					// A choice on a missing occurrence stays saved but joins nothing.
					delete(nodes, endpoint.ref())
					candidates = slices.DeleteFunc(candidates, func(c meetingDecisionEdge) bool { return c.a.ref() == endpoint.ref() || c.b.ref() == endpoint.ref() })
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
			pending = append(pending, endpoint)
		}
		choices, err := s.meetingDecisionEdgesBatch(ctx, pending, budget)
		if err != nil {
			return nil, err
		}
		for _, endpoint := range pending {
			for _, choice := range choices[endpoint] {
				if seenDecisions[choice.id] {
					continue
				}
				a, b, ok, err := s.meetingPairKeys(ctx, choice.a, choice.b, budget)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				if a != endpoint && b != endpoint || a == b {
					continue
				}
				seenDecisions[choice.id] = true
				choice.a, choice.b = a, b
				add(choice.a, nil)
				add(choice.b, nil)
				candidates = append(candidates, choice)
			}
		}
	}
	return &meetingDecisionGraph{nodes: nodes, groups: groups, warnings: warnings, candidates: candidates, automatic: automatic}, nil
}

func (g *meetingDecisionGraph) reduce() (func(string) string, []meetingDecisionEdge) {
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
