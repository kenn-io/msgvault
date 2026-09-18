package imazingcsv

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

type evidenceRange struct {
	start int
	end   int
}

func resolveReplies(
	ctx context.Context,
	st *store.Store,
	sourceID int64,
	sourceIdentifier, timezone string,
	owner participantIdentity,
	plans []*plannedMessage,
	includeArchived bool,
) (linked, unresolved int, err error) {
	messageIDsWithoutEvidence := make([]int64, 0, len(plans))
	for _, plan := range plans {
		if strings.TrimSpace(plan.row.ReplyingTo) == "" {
			messageIDsWithoutEvidence = append(messageIDsWithoutEvidence, plan.messageID)
		}
	}
	if err := st.ClearMessageRepliesContext(ctx, sourceID, messageIDsWithoutEvidence); err != nil {
		return 0, 0, fmt.Errorf("clear iMazing CSV reply links: %w", err)
	}

	// Matching every evidence row against every message makes large imports
	// quadratic, so reply resolution works per conversation from an index of
	// the earlier messages whose rendered dates and senders could possibly
	// appear in the evidence text.
	conversations := make(map[string][]*plannedMessage)
	conversationOrder := make([]string, 0)
	withEvidence := make(map[string]bool)
	var archivedConversations map[string][]*plannedMessage
	for _, plan := range plans {
		if _, ok := conversations[plan.chat.key]; !ok {
			conversationOrder = append(conversationOrder, plan.chat.key)
		}
		conversations[plan.chat.key] = append(conversations[plan.chat.key], plan)
		if strings.TrimSpace(plan.row.ReplyingTo) != "" {
			withEvidence[plan.chat.key] = true
		}
	}
	evidenceConversations := make([]string, 0, len(withEvidence))
	for _, key := range conversationOrder {
		if withEvidence[key] {
			evidenceConversations = append(evidenceConversations, key)
		}
	}
	for _, key := range conversationOrder {
		if !withEvidence[key] {
			continue
		}
		index := newReplyCandidateIndex(conversations[key])
		if includeArchived {
			if archivedConversations == nil {
				archivedConversations, err = loadArchivedReplyPlans(
					ctx, st, sourceID, sourceIdentifier, timezone, owner, evidenceConversations,
				)
				if err != nil {
					return linked, unresolved, err
				}
			}
			index = newReplyCandidateIndex(archivedConversations[key])
		}
		for _, child := range conversations[key] {
			if err := ctx.Err(); err != nil {
				return linked, unresolved, err
			}
			if strings.TrimSpace(child.row.ReplyingTo) == "" {
				continue
			}
			var candidates []*plannedMessage
			for _, parent := range index.candidates(child) {
				if replyEvidenceMatches(child.row.ReplyingTo, parent) {
					candidates = append(candidates, parent)
				}
			}
			if len(candidates) == 0 {
				if err := st.ClearMessageRepliesContext(ctx, sourceID, []int64{child.messageID}); err != nil {
					return linked, unresolved, fmt.Errorf("clear unresolved iMazing CSV reply for %s: %w",
						child.sourceMessageID, err)
				}
				unresolved++
				continue
			}
			if len(candidates) != 1 {
				if err := st.ClearMessageRepliesContext(ctx, sourceID, []int64{child.messageID}); err != nil {
					return linked, unresolved, fmt.Errorf("clear ambiguous iMazing CSV reply for %s: %w",
						child.sourceMessageID, err)
				}
				unresolved++
				continue
			}
			if err := st.SetMessageReplyContext(ctx, child.messageID, candidates[0].messageID); err != nil {
				return linked, unresolved, fmt.Errorf("link iMazing CSV reply for %s: %w", child.sourceMessageID, err)
			}
			linked++
		}
	}
	return linked, unresolved, nil
}

func loadArchivedReplyPlans(
	ctx context.Context,
	st *store.Store,
	sourceID int64,
	sourceIdentifier, timezone string,
	owner participantIdentity,
	conversationKeys []string,
) (map[string][]*plannedMessage, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("load archived reply timezone %q: %w", timezone, err)
	}
	conversations := make(map[string][]*plannedMessage)
	const pageSize = 1000
	for _, scopedKey := range conversationKeys {
		var afterID int64
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			archived, err := st.ScanArchivedRawMessagesForConversation(
				sourceID, scopedKey, RawFormat, afterID, pageSize,
			)
			if err != nil {
				return nil, fmt.Errorf("scan archived iMazing CSV reply candidates: %w", err)
			}
			if len(archived) == 0 {
				break
			}
			for _, item := range archived {
				var raw map[string]string
				if err := json.Unmarshal(item.RawData, &raw); err != nil {
					return nil, fmt.Errorf("decode archived iMazing CSV message %d: %w", item.MessageID, err)
				}
				row, err := normalizeArchivedReplyRow(raw, loc)
				if err != nil {
					return nil, fmt.Errorf("normalize archived iMazing CSV message %d: %w", item.MessageID, err)
				}
				row.Raw = nil
				chatKey := conversationKey(row.ChatSession)
				sender := owner
				if row.Direction == DirectionIncoming {
					switch {
					case row.SenderID != "":
						sender, err = normalizeIdentity(row.SenderID, row.SenderName)
					case row.SenderName != "":
						sender = syntheticIdentity(sourceIdentifier, chatKey, "sender", row.SenderName)
					default:
						err = errors.New("incoming sender ID or name is required")
					}
					if err != nil {
						return nil, fmt.Errorf("normalize archived iMazing CSV sender for message %d: %w",
							item.MessageID, err)
					}
				}
				conversations[chatKey] = append(conversations[chatKey], &plannedMessage{
					row: row, sender: sender, messageID: item.MessageID,
				})
				afterID = item.MessageID
			}
		}
	}
	return conversations, nil
}

func normalizeArchivedReplyRow(raw map[string]string, loc *time.Location) (Row, error) {
	headers := make([]string, 0, len(raw))
	for header := range raw {
		headers = append(headers, header)
	}
	sort.Strings(headers)
	values := make([]string, len(headers))
	indexes := make(map[string]int, len(headers))
	for index, header := range headers {
		values[index] = raw[header]
		indexes[normalizeHeader(header)] = index
	}
	return normalizeRow(values, indexes, headers, "archived", 0, loc)
}

// indexedParent is one conversation message that reply evidence can quote,
// together with the needle identifiers of its non-empty sender values.
type indexedParent struct {
	plan          *plannedMessage
	senderNeedles []int
}

// replyCandidateIndex groups one conversation's messages by their rendered
// date so a child's reply evidence only has to verify the handful of earlier
// messages whose dates actually occur in the evidence text. Both the date and
// a sender value of the parent must occur in the evidence for a match, and
// the full replyEvidenceMatches check still decides the final answer, so the
// index only ever restricts — it never changes which parents can match or
// how ambiguity is counted.
type replyCandidateIndex struct {
	matcher     *evidenceNeedleMatcher
	dateParents [][]indexedParent
}

func newReplyCandidateIndex(plans []*plannedMessage) *replyCandidateIndex {
	needleIDs := make(map[string]int)
	needles := make([]string, 0, len(plans))
	ensureNeedle := func(value string) int {
		if value == "" {
			return -1
		}
		if id, ok := needleIDs[value]; ok {
			return id
		}
		id := len(needles)
		needles = append(needles, value)
		needleIDs[value] = id
		return id
	}
	index := &replyCandidateIndex{}
	for _, plan := range plans {
		dateID := ensureNeedle(normalizeRenderedEvidence(plan.row.MessageDate))
		if dateID < 0 {
			// A message without a rendered date can never satisfy the exact
			// date match, so it never becomes a candidate.
			continue
		}
		senderValues := []string{
			plan.row.SenderName, plan.row.SenderID,
			plan.sender.DisplayName, plan.sender.Value,
		}
		for i := range senderValues {
			senderValues[i] = normalizeRenderedEvidence(senderValues[i])
		}
		slices.Sort(senderValues)
		senderValues = slices.Compact(senderValues)
		senderNeedles := make([]int, 0, len(senderValues))
		for _, value := range senderValues {
			if value == "" {
				continue
			}
			if id := ensureNeedle(value); id >= 0 {
				senderNeedles = append(senderNeedles, id)
			}
		}
		if len(senderNeedles) == 0 {
			// Without any sender value the exact sender match cannot hold
			// either.
			continue
		}
		for len(index.dateParents) <= dateID {
			index.dateParents = append(index.dateParents, nil)
		}
		index.dateParents[dateID] = append(index.dateParents[dateID],
			indexedParent{plan: plan, senderNeedles: senderNeedles})
	}
	index.matcher = newEvidenceNeedleMatcher(needles)
	return index
}

// candidates returns the conversation's earlier messages whose rendered date
// and at least one sender value occur in the child's reply evidence, in
// needle-discovery order within each matched date. It walks only the matched
// needle IDs, so its cost scales with the hits in the evidence text and the
// parents sharing those dates, never with the number of distinct dates.
func (index *replyCandidateIndex) candidates(child *plannedMessage) []*plannedMessage {
	rendered := normalizeRenderedEvidence(child.row.ReplyingTo)
	matches := index.matcher.match(rendered)
	var result []*plannedMessage
	for _, dateID := range matches.matchedIDs {
		if dateID >= len(index.dateParents) {
			// A sender-only needle that no date bucket uses.
			continue
		}
		for _, parent := range index.dateParents[dateID] {
			if !parent.plan.row.SentAt.Before(child.row.SentAt) {
				continue
			}
			if !anyNeedleMatched(matches.matched, parent.senderNeedles) {
				continue
			}
			result = append(result, parent.plan)
		}
	}
	return result
}

func anyNeedleMatched(matched []bool, needles []int) bool {
	for _, id := range needles {
		if matched[id] {
			return true
		}
	}
	return false
}

// evidenceNeedleMatcher is a small Aho–Corasick automaton that reports every
// fixed evidence string occurring anywhere inside one rendered text in a
// single pass. Reply resolution uses it to find which conversation dates and
// senders a reply-evidence text contains without scanning every message.
type evidenceNeedleMatcher struct {
	transitions []map[byte]int
	fail        []int
	dictLink    []int
	needleIDs   [][]int
	needleCount int
}

func newEvidenceNeedleMatcher(needles []string) *evidenceNeedleMatcher {
	matcher := &evidenceNeedleMatcher{
		transitions: []map[byte]int{{}},
		needleIDs:   [][]int{nil},
		needleCount: len(needles),
	}
	for id, needle := range needles {
		var node int
		for i := range len(needle) {
			c := needle[i]
			child, ok := matcher.transitions[node][c]
			if !ok {
				child = len(matcher.transitions)
				matcher.transitions = append(matcher.transitions, map[byte]int{})
				matcher.needleIDs = append(matcher.needleIDs, nil)
				matcher.transitions[node][c] = child
			}
			node = child
		}
		matcher.needleIDs[node] = append(matcher.needleIDs[node], id)
	}
	matcher.fail = make([]int, len(matcher.transitions))
	matcher.dictLink = make([]int, len(matcher.transitions))
	queue := make([]int, 0, len(matcher.transitions))
	for _, child := range matcher.transitions[0] {
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if len(matcher.needleIDs[matcher.fail[node]]) > 0 {
			matcher.dictLink[node] = matcher.fail[node]
		} else {
			matcher.dictLink[node] = matcher.dictLink[matcher.fail[node]]
		}
		for c, child := range matcher.transitions[node] {
			fail := matcher.fail[node]
			for fail != 0 {
				if _, ok := matcher.transitions[fail][c]; ok {
					break
				}
				fail = matcher.fail[fail]
			}
			if next, ok := matcher.transitions[fail][c]; ok && next != child {
				matcher.fail[child] = next
			} else {
				matcher.fail[child] = 0
			}
			queue = append(queue, child)
		}
	}
	return matcher
}

// evidenceNeedleMatches reports the needles that occur anywhere in one
// rendered text. matched maps needle ID to membership, and matchedIDs lists
// each matched needle ID exactly once, so callers enumerate only the hits
// instead of every indexed needle.
type evidenceNeedleMatches struct {
	matched    []bool
	matchedIDs []int
}

// match reports which needles occur anywhere in text. Occurrences found here
// ignore the word-boundary rules the full evidence matcher applies, so the
// result is always a superset of the exact matches.
func (matcher *evidenceNeedleMatcher) match(text string) evidenceNeedleMatches {
	result := evidenceNeedleMatches{matched: make([]bool, matcher.needleCount)}
	var node int
	for i := range len(text) {
		c := text[i]
		for node != 0 {
			if _, ok := matcher.transitions[node][c]; ok {
				break
			}
			node = matcher.fail[node]
		}
		if child, ok := matcher.transitions[node][c]; ok {
			node = child
		} else {
			node = 0
		}
		for hit := node; hit != 0; hit = matcher.dictLink[hit] {
			for _, id := range matcher.needleIDs[hit] {
				if result.matched[id] {
					continue
				}
				result.matched[id] = true
				result.matchedIDs = append(result.matchedIDs, id)
			}
		}
	}
	return result
}

func replyEvidenceMatches(rendered string, parent *plannedMessage) bool {
	if parent == nil {
		return false
	}
	text := normalizeRenderedEvidence(rendered)
	body := normalizeRenderedEvidence(parent.row.Text)
	date := normalizeRenderedEvidence(parent.row.MessageDate)
	if text == "" || body == "" || date == "" {
		return false
	}
	bodyRanges := exactEvidenceRanges(text, body)
	dateRanges := exactEvidenceRanges(text, date)
	if len(bodyRanges) == 0 || len(dateRanges) == 0 {
		return false
	}

	senderValues := []string{parent.row.SenderName, parent.row.SenderID, parent.sender.DisplayName, parent.sender.Value}
	for index := range senderValues {
		senderValues[index] = normalizeRenderedEvidence(senderValues[index])
	}
	slices.Sort(senderValues)
	senderValues = slices.Compact(senderValues)
	for _, sender := range senderValues {
		if sender == "" {
			continue
		}
		for _, senderRange := range exactEvidenceRanges(text, sender) {
			for _, bodyRange := range bodyRanges {
				for _, dateRange := range dateRanges {
					if rangesDisjoint(senderRange, bodyRange) &&
						rangesDisjoint(senderRange, dateRange) &&
						rangesDisjoint(bodyRange, dateRange) {
						return true
					}
				}
			}
		}
	}
	return false
}

func normalizeRenderedEvidence(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func exactEvidenceRanges(text, evidence string) []evidenceRange {
	if evidence == "" {
		return nil
	}
	var result []evidenceRange
	for offset := 0; offset <= len(text)-len(evidence); {
		index := strings.Index(text[offset:], evidence)
		if index < 0 {
			break
		}
		start := offset + index
		end := start + len(evidence)
		if evidenceBoundary(text, start, end) {
			result = append(result, evidenceRange{start: start, end: end})
		}
		offset = start + 1
	}
	return result
}

func evidenceBoundary(text string, start, end int) bool {
	first, _ := utf8.DecodeRuneInString(text[start:end])
	last, _ := utf8.DecodeLastRuneInString(text[start:end])
	if start > 0 && (unicode.IsLetter(first) || unicode.IsDigit(first)) {
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		if unicode.IsLetter(before) || unicode.IsDigit(before) {
			return false
		}
	}
	if end < len(text) && (unicode.IsLetter(last) || unicode.IsDigit(last)) {
		after, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsLetter(after) || unicode.IsDigit(after) {
			return false
		}
	}
	return true
}

func rangesDisjoint(left, right evidenceRange) bool {
	return left.end <= right.start || right.end <= left.start
}
