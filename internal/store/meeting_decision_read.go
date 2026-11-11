package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/calrecur"
)

type meetingDecisionEdge struct {
	a, b     meetingDecisionEndpoint
	decision string
}

type meetingDecisionConflict struct {
	link, split meetingDecisionEdge
}

func parseStoredMeetingRef(value string) (meetingDecisionEndpoint, error) {
	kind, rest, ok := strings.Cut(value, ":")
	if !ok {
		return meetingDecisionEndpoint{}, fmt.Errorf("invalid meeting ref %q", value)
	}
	idText, key, _ := strings.Cut(rest, "@")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return meetingDecisionEndpoint{}, err
	}
	return meetingDecisionEndpoint{ID: id, Kind: kind, Key: key}, nil
}

func (s *Store) meetingDecisionsForMessage(ctx context.Context, id int64, budget *int) ([]meetingDecisionEdge, error) {
	leftScope, leftArgs := sourceScopeClause(ctx, "l.source_id")
	rightScope, rightArgs := sourceScopeClause(ctx, "r.source_id")
	args := []any{id, id}
	args = append(args, leftArgs...)
	args = append(args, rightArgs...)
	args = append(args, *budget+1)
	rows, err := s.db.QueryContext(ctx, `SELECT d.left_message_id,d.left_kind,d.left_key,d.left_anchor,d.right_message_id,d.right_kind,d.right_key,d.right_anchor,d.decision FROM meeting_link_decisions d JOIN messages l ON l.id=d.left_message_id JOIN messages r ON r.id=d.right_message_id WHERE (d.left_message_id=? OR d.right_message_id=?) AND l.deleted_at IS NULL AND r.deleted_at IS NULL`+leftScope+rightScope+` LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []meetingDecisionEdge
	for rows.Next() {
		var edge meetingDecisionEdge
		if err := rows.Scan(&edge.a.ID, &edge.a.Kind, &edge.a.Key, &edge.a.Anchor, &edge.b.ID, &edge.b.Kind, &edge.b.Key, &edge.b.Anchor, &edge.decision); err != nil {
			return nil, err
		}
		out = append(out, edge)
		*budget--
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if *budget < 0 {
		return nil, calendarRecurrenceBudgetError()
	}
	return out, nil
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
	var automatic, explicit, splits []meetingDecisionEdge
	seenDecisions := map[string]bool{}
	for len(queue) > 0 {
		endpoint := queue[0]
		queue = queue[1:]
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
		result.Unexpandable = append(result.Unexpandable, record.Unexpandable...)
		var members []meetingDecisionEndpoint
		for _, id := range record.MessageIDs {
			member := meetingDecisionEndpoint{ID: id, Kind: "msg"}
			add(member, nil)
			members = append(members, member)
		}
		for _, link := range record.Links {
			member, err := parseStoredMeetingRef(link.Ref)
			if err != nil {
				return result, err
			}
			copy := link
			add(member, &copy)
			members = append(members, member)
			if link.MessageID != member.ID {
				alias := meetingDecisionEndpoint{ID: link.MessageID, Kind: "evt"}
				add(alias, nil)
				automatic = append(automatic, meetingDecisionEdge{a: alias, b: member})
			}
			if endpoint.Kind == "evt" && link.MessageID == endpoint.ID {
				automatic = append(automatic, meetingDecisionEdge{a: endpoint, b: member})
			}
		}
		for _, member := range members {
			automatic = append(automatic, meetingDecisionEdge{a: endpoint, b: member})
		}
		choices, err := s.meetingDecisionsForMessage(ctx, endpoint.ID, &budget)
		if err != nil {
			return result, err
		}
		for _, choice := range choices {
			key := choice.a.ref() + "/" + choice.b.ref()
			if seenDecisions[key] {
				continue
			}
			seenDecisions[key] = true
			add(choice.a, nil)
			add(choice.b, nil)
			if choice.decision == "split" {
				splits = append(splits, choice)
			} else {
				explicit = append(explicit, choice)
			}
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
	// A note/event split applies to all automatic event copies of that occurrence.
	for _, split := range slices.Clone(splits) {
		note, event := split.a, split.b
		if note.Kind == "evt" {
			note, event = event, note
		}
		if note.Kind != "msg" || event.Kind != "evt" {
			continue
		}
		for _, edge := range automatic {
			if edge.a.ref() == event.ref() && edge.b.Kind == "evt" {
				splits = append(splits, meetingDecisionEdge{a: note, b: edge.b})
			}
			if edge.b.ref() == event.ref() && edge.a.Kind == "evt" {
				splits = append(splits, meetingDecisionEdge{a: note, b: edge.a})
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
	order(automatic)
	for _, edge := range append(explicit, automatic...) {
		a, b := root(edge.a.ref()), root(edge.b.ref())
		if a == b {
			continue
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
	seedRoot := root(seed.ref())
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
