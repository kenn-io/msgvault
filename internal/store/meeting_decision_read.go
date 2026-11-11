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

type meetingDecisionConflict struct {
	link, split meetingDecisionEdge
}

func (s *Store) meetingDecisionsForMessage(ctx context.Context, id int64, budget *int) ([]meetingDecisionEdge, error) {
	loader := meetingDecisionLoader{store: s}
	return loader.edges(ctx, meetingDecisionEndpoint{ID: id}, nil, budget)
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

// meetingDecisionEndpointClause filters occurrence keys before the read budget applies.
func meetingDecisionEndpointClause(endpoints []meetingDecisionEndpoint) (string, []any) {
	var clauses []string
	var args []any
	for _, endpoint := range endpoints {
		for _, side := range []string{"left", "right"} {
			clause := "d." + side + "_message_id=?"
			args = append(args, endpoint.ID)
			if endpoint.Kind != "" {
				clause += " AND d." + side + "_kind=?"
				args = append(args, endpoint.Kind)
				if endpoint.Kind == "evt" {
					clause += " AND (d." + side + "_key=? OR (d." + side + "_key='' AND d." + side + "_anchor=?))"
					args = append(args, endpoint.Key, endpoint.Key)
				}
			}
			clauses = append(clauses, "("+clause+")")
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
	return query.ID == stored.ID && (query.Kind == "" || query.Kind == stored.Kind && (query.Kind != "evt" || query.Key == stored.Key || stored.Key == "" && stored.Anchor == query.Key))
}

// MeetingRecordContext follows automatic evidence and owner choices in one snapshot.
func (s *Store) MeetingRecordContext(ctx context.Context, ref MeetingLinkRef) (MeetingRecord, error) {
	return s.meetingRecordContext(ctx, ref, nil)
}

func (s *Store) meetingRecordContext(ctx context.Context, ref MeetingLinkRef, conflict *meetingDecisionConflict) (MeetingRecord, error) {
	result := MeetingRecord{Links: []MeetingLink{}, MessageIDs: []int64{}, Unexpandable: []CalendarUnexpandable{}}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	budget := calrecur.RequestReadBudget
	type node struct {
		endpoint meetingDecisionEndpoint
		link     *MeetingLink
	}
	nodes := map[string]node{}
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
		nodes[key] = node{endpoint, link}
		queue = append(queue, endpoint)
	}
	seed := endpointFromRef(ref)
	add(seed, nil)
	type warningSource struct {
		endpoint meetingDecisionEndpoint
		warning  CalendarUnexpandable
	}
	var warnings []warningSource
	var groups [][]meetingDecisionEndpoint
	var explicit, splits []meetingDecisionEdge
	var automatic MeetingRecord
	seenDecisions := map[int64]bool{}
	var candidates []meetingDecisionEdge
	resolved := map[string]bool{}
	decisions := meetingDecisionLoader{store: s}
	for len(queue) > 0 {
		endpoint := queue[0]
		queue = queue[1:]
		if !resolved[endpoint.ref()] {
			itemRef, err := endpoint.meetingRef()
			if err != nil {
				return result, err
			}
			record, err := s.meetingAutomaticRecordContext(ctx, itemRef, &budget)
			if errors.Is(err, ErrMeetingOccurrenceRequired) && endpoint.Anchor != "" {
				anchored := endpoint
				anchored.Key = anchored.Anchor
				itemRef, err = anchored.meetingRef()
				if err == nil {
					record, err = s.meetingAutomaticRecordContext(ctx, itemRef, &budget)
				}
			}
			if errors.Is(err, sql.ErrNoRows) && endpoint != seed {
				continue
			}
			if err != nil {
				return result, err
			}
			if endpoint == seed {
				automatic = record
			}
			for _, warning := range record.Unexpandable {
				warnings = append(warnings, warningSource{endpoint, warning})
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
				member := meetingDecisionEndpoint{ID: id, Kind: "msg"}
				add(member, nil)
				addMember(member)
			}
			for _, link := range record.Links {
				ref, err := ParseMeetingLinkRef(link.Ref)
				if err != nil {
					return result, err
				}
				member := endpointFromRef(ref)
				copy := link
				add(member, &copy)
				addMember(member)
				if link.MessageID != member.ID {
					alias := meetingDecisionEndpoint{ID: link.MessageID, Kind: "evt"}
					add(alias, nil)
					addMember(alias)
				}
			}
			for _, member := range members {
				resolved[member.ref()] = true
			}
			groups = append(groups, members)
		}
		choices, err := decisions.edges(ctx, endpoint, queue, &budget)
		if err != nil {
			return result, err
		}
		for _, choice := range choices {
			if seenDecisions[choice.id] {
				continue
			}
			seenDecisions[choice.id] = true
			add(choice.a, nil)
			add(choice.b, nil)
			candidates = append(candidates, choice)
		}
	}
	if len(candidates) == 0 {
		return automatic, nil
	}
	// Materialize aliases before choosing one effective contribution per pair.
	effective := map[string]meetingDecisionEdge{}
	for _, choice := range candidates {
		left, _, err := s.materializedMeetingEndpoint(ctx, choice.a, &budget)
		if errors.Is(err, sql.ErrNoRows) {
			left, err = choice.a, nil
		}
		if err != nil {
			return result, err
		}
		right, _, err := s.materializedMeetingEndpoint(ctx, choice.b, &budget)
		if errors.Is(err, sql.ErrNoRows) {
			right, err = choice.b, nil
		}
		if err != nil {
			return result, err
		}
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
		add(choice.a, nil)
		add(choice.b, nil)
		if choice.decision == "split" {
			splits = append(splits, choice)
		} else {
			explicit = append(explicit, choice)
		}
	}
	parent := map[string]string{}
	for key := range nodes {
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
	for _, group := range groups {
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
		if note.Kind == "evt" {
			note, event = event, note
		}
		if note.Kind != "msg" || event.Kind != "evt" {
			continue
		}
		for _, group := range groupByMember[event.ref()] {
			for _, member := range group {
				if member.Kind == "evt" && member.ref() != event.ref() {
					splits = append(splits, meetingDecisionEdge{a: note, b: member})
				}
			}
		}
	}
	order := func(edges []meetingDecisionEdge) {
		slices.SortFunc(edges, func(a, b meetingDecisionEdge) int {
			if compareMeetingEndpoints(a.a, a.b) > 0 {
				a.a, a.b = a.b, a.a
			}
			if compareMeetingEndpoints(b.a, b.b) > 0 {
				b.a, b.b = b.b, b.a
			}
			if c := compareMeetingEndpoints(a.a, b.a); c != 0 {
				return c
			}
			return compareMeetingEndpoints(a.b, b.b)
		})
	}
	order(explicit)
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
				if edge.decision == "link" && conflict != nil {
					*conflict = meetingDecisionConflict{link: edge, split: split}
				}
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
	if len(splits) == 0 {
		for _, group := range groups {
			for _, member := range group[1:] {
				join(meetingDecisionEdge{a: group[0], b: member})
			}
		}
	} else {
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
	}
	seedRoot := root(seed.ref())
	visibleEvents := map[int64]bool{}
	for key, n := range nodes {
		if n.endpoint.Kind == "evt" {
			visibleEvents[n.endpoint.ID] = visibleEvents[n.endpoint.ID] || root(key) == seedRoot
		}
	}
	// Keep a warning with its event when known, or with the group that found it.
	for _, entry := range warnings {
		visible, present := visibleEvents[entry.warning.MessageID]
		if visible || !present && root(entry.endpoint.ref()) == seedRoot {
			result.Unexpandable = append(result.Unexpandable, entry.warning)
		}
	}
	for key, n := range nodes {
		if root(key) != seedRoot {
			continue
		}
		if n.endpoint.Kind == "msg" {
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
	return result, nil
}
