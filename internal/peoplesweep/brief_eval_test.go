package peoplesweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

var briefFixtureNames = []string{
	"simple-catch-up.json",
	"owner-appreciation-and-third-party.json",
	"stale-fact.json",
}

type briefFixture struct {
	Name            string                `json:"name"`
	AllowSensitive  bool                  `json:"allow_sensitive"`
	MaxItems        int                   `json:"max_items"`
	MaxBytes        int                   `json:"max_bytes"`
	OverlapItems    int                   `json:"overlap_items"`
	ThroughSequence int64                 `json:"through_sequence"`
	Messages        []briefFixtureMessage `json:"messages"`
	LastContact     briefFixtureContact   `json:"last_contact"`
	Provider        briefFixtureOutput    `json:"provider"`
	Expected        briefFixtureExpected  `json:"expected"`
}

type briefFixtureMessage struct {
	Key       string `json:"key"`
	Lane      string `json:"lane"`
	Author    string `json:"author"`
	EventTime string `json:"event_time"`
	Excerpt   string `json:"excerpt"`
}

type briefFixtureContact struct {
	Message string `json:"message"`
	Channel string `json:"channel"`
}

type briefFixtureOutput struct {
	LastMeaningfulInteraction *briefFixtureInteraction   `json:"last_meaningful_interaction"`
	Highlights                []briefFixtureHighlight    `json:"highlights"`
	FollowUps                 []briefFixtureFollowUp     `json:"follow_ups"`
	Appreciations             []briefFixtureAppreciation `json:"appreciations"`
	Uncertainties             []briefFixtureUncertainty  `json:"uncertainties"`
	PossibleAttributes        []briefFixtureAttribute    `json:"possible_attributes"`
}

type briefFixtureInteraction struct {
	Evidence string `json:"evidence"`
	Summary  string `json:"summary"`
}

type briefFixtureHighlight struct {
	Text                  string   `json:"text"`
	Speaker               string   `json:"speaker"`
	Evidence              []string `json:"evidence"`
	ObservedAt            *string  `json:"observed_at"`
	ConfidenceBasisPoints int      `json:"confidence_basis_points"`
}

type briefFixtureFollowUp struct {
	Question       string   `json:"question"`
	Why            string   `json:"why"`
	HighlightIndex int      `json:"highlight_index"`
	Evidence       []string `json:"evidence"`
}

type briefFixtureAppreciation struct {
	Text     string   `json:"text"`
	Evidence []string `json:"evidence"`
}

type briefFixtureUncertainty struct {
	Text     string   `json:"text"`
	Kind     string   `json:"kind"`
	Evidence []string `json:"evidence"`
}

type briefFixtureAttribute struct {
	TargetSlug            string          `json:"target_slug"`
	Relation              string          `json:"relation"`
	Value                 json.RawMessage `json:"value"`
	Evidence              []string        `json:"evidence"`
	ValidFrom             *string         `json:"valid_from"`
	ValidUntil            *string         `json:"valid_until"`
	ConfidenceBasisPoints int             `json:"confidence_basis_points"`
}

type briefFixtureExpected struct {
	WindowMessages   []string           `json:"window_messages"`
	Lanes            []string           `json:"lanes"`
	FromEventTime    string             `json:"from_event_time"`
	ThroughEventTime string             `json:"through_event_time"`
	DroppedItemCount int                `json:"dropped_item_count"`
	EvidenceOrder    []string           `json:"evidence_order"`
	Output           briefFixtureOutput `json:"output"`
	RenderedText     string             `json:"rendered_text"`
}

// TestPersonBriefFrozenEvaluation runs each frozen fixture through the real
// retrieval, window, parser, and renderer against a real archive. The assertion
// is on the validated structure and the deterministic paragraph, never on
// provider prose.
func TestPersonBriefFrozenEvaluation(t *testing.T) {
	for _, name := range briefFixtureNames {
		fixture := loadBriefFixture(t, name)
		t.Run(fixture.Name, func(t *testing.T) {
			runBriefFixture(t, fixture)
		})
	}
}

func loadBriefFixture(t *testing.T, name string) briefFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "brief", name))
	require.NoError(t, err, name)
	var fixture briefFixture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&fixture), name)
	require.NotEmpty(t, fixture.Name, name)
	require.NotEmpty(t, fixture.Messages, name)
	require.Positive(t, fixture.MaxItems, name)
	return fixture
}

func runBriefFixture(t *testing.T, fixture briefFixture) {
	t.Helper()
	f := newEvaluationStore(t)
	messageIDs := make(map[string]int64, len(fixture.Messages))
	for index, message := range fixture.Messages {
		messageIDs[message.Key] = insertBriefMessage(t, f, index, message)
	}
	seedBriefContactState(t, f, fixture, messageIDs)

	catalog, err := f.Store.BuildPersonFactCatalogContext(t.Context(), fixture.AllowSensitive)
	require.NoError(t, err)
	profile := peoplesweep.ProviderProfile{
		Fingerprint: "frozen-brief-policy-v1",
		AllowedSources: []peoplesweep.SourceClass{
			peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText},
		SourceSince: "2000-01-01", AllowSensitive: fixture.AllowSensitive,
	}

	window, err := peoplesweep.BuildBriefWindow(t.Context(), f.Store, peoplesweep.BriefWindowRequest{
		PersonID: f.PersonID, Catalog: catalog, Profile: profile,
		MaxItems: fixture.MaxItems, MaxBytes: fixture.MaxBytes,
		OverlapItems: fixture.OverlapItems, ThroughSequence: fixture.ThroughSequence,
	})
	require.NoError(t, err)

	assert.Equal(t, briefFixtureKeys(t, messageIDs, fixture.Expected.WindowMessages),
		briefWindowKeys(t, fixture, messageIDs, window),
		"the window holds exactly the person's own in-policy items, newest first")
	assert.Equal(t, fixture.Expected.Lanes, window.Boundary.Lanes)
	for _, message := range fixture.Messages {
		if message.Lane == string(peoplesweep.SourceMeetingText) {
			assert.NotContains(t, briefWindowKeys(t, fixture, messageIDs, window), message.Key,
				"a meeting transcript stays out of the v1 window even on an allowing profile (R13)")
		}
	}
	assert.Equal(t, briefFixtureTime(t, fixture.Expected.FromEventTime), window.Boundary.FromEventTime)
	assert.Equal(t, briefFixtureTime(t, fixture.Expected.ThroughEventTime), window.Boundary.ThroughEventTime)
	assert.Equal(t, fixture.ThroughSequence, window.Boundary.ThroughSequence)
	assert.Equal(t, len(window.Items), window.Boundary.ItemCount)
	assert.NotEmpty(t, window.Boundary.PacketSHA256)
	assert.True(t, window.Batch.Request.ContainsSensitive)

	targets := evaluationTargetsBySlug(catalog)
	evidenceIDs := briefEvidenceIDs(t, window, messageIDs)
	providerJSON := briefProviderJSON(t, fixture.Provider, evidenceIDs, targets)

	parsed, err := peoplesweep.ParseBrief(providerJSON, window, profile)
	require.NoError(t, err)
	assert.Equal(t, fixture.Expected.DroppedItemCount, parsed.DroppedItemCount)
	assertBriefOutput(t, fixture.Expected.Output, parsed.Output, evidenceIDs, targets)

	expectedEvidence := make([]string, 0, len(fixture.Expected.EvidenceOrder))
	for _, key := range fixture.Expected.EvidenceOrder {
		expectedEvidence = append(expectedEvidence, evidenceIDs[key])
	}
	actualEvidence := make([]string, 0, len(parsed.Evidence))
	for _, evidence := range parsed.Evidence {
		ref, refErr := peoplesweep.DecodePersonSweepEvidenceRef(evidence.SourceRef)
		require.NoError(t, refErr)
		actualEvidence = append(actualEvidence, evidenceIDs[briefKeyForMessage(fixture, messageIDs, ref.MessageID)])
		assert.Equal(t, f.PersonID, evidence.PersonID)
		assert.Equal(t, personfacts.EvidenceArchive, evidence.SourceClass)
	}
	assert.Equal(t, expectedEvidence, actualEvidence, "evidence pointers follow citation order")

	for _, claim := range parsed.Claims {
		assert.Equal(t, personfacts.OriginBrief, claim.Origin)
	}

	rendered, err := peoplesweep.RenderBrief(parsed, window)
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.BriefRendererPolicyV1, rendered.Policy)
	assert.Equal(t, fixture.Expected.RenderedText, rendered.Text)
	assert.Len(t, rendered.Sentences, briefExpectedSentenceCount(fixture.Expected.Output),
		"one rendered sentence per surviving structured item")
}

func insertBriefMessage(
	t *testing.T, f evaluationStore, index int, message briefFixtureMessage,
) int64 {
	t.Helper()
	senderID := f.ScopedParticipant
	if message.Author == "other" {
		senderID = f.OtherParticipant
	} else {
		require.Equal(t, "person", message.Author)
	}
	messageType := "chat"
	if message.Lane == string(peoplesweep.SourceMeetingText) {
		messageType = "meeting_transcript"
	}
	var messageID int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		INSERT INTO messages
			(source_id, source_message_id, conversation_id, message_type, sender_id, sent_at, subject)
		VALUES (?, ?, ?, ?, ?, ?, '')
		RETURNING id`), f.SourceID, fmt.Sprintf("brief-%02d", index), f.ConversationID,
		messageType, senderID, briefFixtureTime(t, message.EventTime)).Scan(&messageID))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`),
		messageID, message.Excerpt)
	require.NoError(t, err)
	if message.Author == "other" {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
			VALUES (?, ?, 'to', 'alice@example.test')`), messageID, f.ScopedParticipant)
		require.NoError(t, err)
	}
	return messageID
}

// seedBriefContactState writes the deterministic contact projection the brief
// window reads. The projection itself is covered by the store's activity tests;
// here it is fixture input, so the window and renderer run against a real row.
func seedBriefContactState(
	t *testing.T, f evaluationStore, fixture briefFixture, messageIDs map[string]int64,
) {
	t.Helper()
	if fixture.LastContact.Message == "" {
		return
	}
	messageID, ok := messageIDs[fixture.LastContact.Message]
	require.True(t, ok, "last_contact names an unknown message")
	var eventTime time.Time
	for _, message := range fixture.Messages {
		if message.Key == fixture.LastContact.Message {
			eventTime = briefFixtureTime(t, message.EventTime)
		}
	}
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		INSERT INTO person_contact_state
			(person_id, last_contact_at, last_contact_message_id, last_contact_channel,
			 last_contact_source_id, interaction_count, computed_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)`),
		f.PersonID, eventTime, messageID, fixture.LastContact.Channel, f.SourceID, eventTime)
	require.NoError(t, err)
}

func briefEvidenceIDs(
	t *testing.T, window peoplesweep.BriefWindow, messageIDs map[string]int64,
) map[string]string {
	t.Helper()
	raw, err := peoplesweep.CanonicalPacketJSON(window.Batch.Packet)
	require.NoError(t, err)
	var wire struct {
		Seeds []struct {
			ID        string `json:"id"`
			SourceRef string `json:"source_ref"`
		} `json:"seeds"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	byMessage := make(map[int64]string, len(wire.Seeds))
	for _, seed := range wire.Seeds {
		ref, refErr := peoplesweep.DecodePersonSweepEvidenceRef(seed.SourceRef)
		require.NoError(t, refErr)
		byMessage[ref.MessageID] = seed.ID
	}
	ids := make(map[string]string, len(messageIDs))
	for key, messageID := range messageIDs {
		if id, ok := byMessage[messageID]; ok {
			ids[key] = id
		}
	}
	return ids
}

func briefProviderJSON(
	t *testing.T,
	output briefFixtureOutput,
	evidenceIDs map[string]string,
	targets map[string]personfacts.TargetDescriptor,
) json.RawMessage {
	t.Helper()
	resolve := func(keys []string) []string {
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			id, ok := evidenceIDs[key]
			require.True(t, ok, "fixture cites %q, which the window does not hold", key)
			ids = append(ids, id)
		}
		return ids
	}
	document := map[string]any{
		"last_meaningful_interaction": nil,
		"highlights":                  []any{},
		"follow_ups":                  []any{},
		"appreciations":               []any{},
		"uncertainties":               []any{},
		"possible_attributes":         []any{},
	}
	if output.LastMeaningfulInteraction != nil {
		document["last_meaningful_interaction"] = map[string]any{
			"evidence_id": resolve([]string{output.LastMeaningfulInteraction.Evidence})[0],
			"summary":     output.LastMeaningfulInteraction.Summary,
		}
	}
	highlights := make([]any, 0, len(output.Highlights))
	for _, highlight := range output.Highlights {
		highlights = append(highlights, map[string]any{
			"text": highlight.Text, "speaker": highlight.Speaker,
			"evidence_ids": resolve(highlight.Evidence), "observed_at": highlight.ObservedAt,
			"confidence_basis_points": highlight.ConfidenceBasisPoints,
		})
	}
	document["highlights"] = highlights
	followUps := make([]any, 0, len(output.FollowUps))
	for _, followUp := range output.FollowUps {
		followUps = append(followUps, map[string]any{
			"question": followUp.Question, "why": followUp.Why,
			"highlight_index": followUp.HighlightIndex, "evidence_ids": resolve(followUp.Evidence),
		})
	}
	document["follow_ups"] = followUps
	appreciations := make([]any, 0, len(output.Appreciations))
	for _, appreciation := range output.Appreciations {
		appreciations = append(appreciations, map[string]any{
			"text": appreciation.Text, "evidence_ids": resolve(appreciation.Evidence),
		})
	}
	document["appreciations"] = appreciations
	uncertainties := make([]any, 0, len(output.Uncertainties))
	for _, uncertainty := range output.Uncertainties {
		uncertainties = append(uncertainties, map[string]any{
			"text": uncertainty.Text, "kind": uncertainty.Kind,
			"evidence_ids": resolve(uncertainty.Evidence),
		})
	}
	document["uncertainties"] = uncertainties
	attributes := make([]any, 0, len(output.PossibleAttributes))
	for _, attribute := range output.PossibleAttributes {
		target, ok := targets[attribute.TargetSlug]
		require.True(t, ok, "fixture target %q is unavailable", attribute.TargetSlug)
		attributes = append(attributes, map[string]any{
			"target_key": target.Key, "relation": attribute.Relation, "value": attribute.Value,
			"evidence_ids": resolve(attribute.Evidence), "valid_from": attribute.ValidFrom,
			"valid_until":             attribute.ValidUntil,
			"confidence_basis_points": attribute.ConfidenceBasisPoints,
		})
	}
	document["possible_attributes"] = attributes

	raw, err := json.Marshal(document)
	require.NoError(t, err)
	return raw
}

func assertBriefOutput(
	t *testing.T,
	want briefFixtureOutput,
	got peoplesweep.BriefOutput,
	evidenceIDs map[string]string,
	targets map[string]personfacts.TargetDescriptor,
) {
	t.Helper()
	resolve := func(keys []string) []string {
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			ids = append(ids, evidenceIDs[key])
		}
		return ids
	}
	if want.LastMeaningfulInteraction == nil {
		assert.Nil(t, got.LastMeaningfulInteraction)
	} else {
		require.NotNil(t, got.LastMeaningfulInteraction)
		assert.Equal(t, evidenceIDs[want.LastMeaningfulInteraction.Evidence],
			got.LastMeaningfulInteraction.EvidenceID)
		assert.Equal(t, want.LastMeaningfulInteraction.Summary, got.LastMeaningfulInteraction.Summary)
	}
	require.Len(t, got.Highlights, len(want.Highlights))
	for index, highlight := range want.Highlights {
		assert.Equal(t, highlight.Text, got.Highlights[index].Text)
		assert.Equal(t, peoplesweep.BriefSpeaker(highlight.Speaker), got.Highlights[index].Speaker)
		assert.Equal(t, resolve(highlight.Evidence), got.Highlights[index].EvidenceIDs)
		assert.Equal(t, highlight.ObservedAt, got.Highlights[index].ObservedAt)
		assert.Equal(t, highlight.ConfidenceBasisPoints, got.Highlights[index].ConfidenceBasisPoints)
	}
	require.Len(t, got.FollowUps, len(want.FollowUps))
	for index, followUp := range want.FollowUps {
		assert.Equal(t, followUp.Question, got.FollowUps[index].Question)
		assert.Equal(t, followUp.Why, got.FollowUps[index].Why)
		assert.Equal(t, followUp.HighlightIndex, got.FollowUps[index].HighlightIndex)
		assert.Equal(t, resolve(followUp.Evidence), got.FollowUps[index].EvidenceIDs)
	}
	require.Len(t, got.Appreciations, len(want.Appreciations))
	for index, appreciation := range want.Appreciations {
		assert.Equal(t, appreciation.Text, got.Appreciations[index].Text)
		assert.Equal(t, resolve(appreciation.Evidence), got.Appreciations[index].EvidenceIDs)
	}
	require.Len(t, got.Uncertainties, len(want.Uncertainties))
	for index, uncertainty := range want.Uncertainties {
		assert.Equal(t, uncertainty.Text, got.Uncertainties[index].Text)
		assert.Equal(t, peoplesweep.BriefUncertaintyKind(uncertainty.Kind), got.Uncertainties[index].Kind)
		assert.Equal(t, resolve(uncertainty.Evidence), got.Uncertainties[index].EvidenceIDs)
	}
	require.Len(t, got.PossibleAttributes, len(want.PossibleAttributes))
	for index, attribute := range want.PossibleAttributes {
		target, ok := targets[attribute.TargetSlug]
		require.True(t, ok)
		assert.Equal(t, target.Key, got.PossibleAttributes[index].TargetKey)
		assert.Equal(t, attribute.Relation, got.PossibleAttributes[index].Relation)
		assert.JSONEq(t, string(attribute.Value), string(got.PossibleAttributes[index].Value))
		assert.Equal(t, resolve(attribute.Evidence), got.PossibleAttributes[index].EvidenceIDs)
	}
}

func briefExpectedSentenceCount(output briefFixtureOutput) int {
	count := len(output.Highlights) + len(output.FollowUps) +
		len(output.Appreciations) + len(output.Uncertainties)
	if output.LastMeaningfulInteraction != nil {
		count++
	}
	return count
}

func briefFixtureKeys(t *testing.T, messageIDs map[string]int64, keys []string) []string {
	t.Helper()
	for _, key := range keys {
		_, ok := messageIDs[key]
		require.True(t, ok, "expected window message %q is not in the fixture", key)
	}
	return keys
}

func briefWindowKeys(
	t *testing.T, fixture briefFixture, messageIDs map[string]int64,
	window peoplesweep.BriefWindow,
) []string {
	t.Helper()
	keys := make([]string, 0, len(window.Items))
	for _, item := range window.Items {
		keys = append(keys, briefKeyForMessage(fixture, messageIDs, item.Ref.MessageID))
	}
	return keys
}

func briefKeyForMessage(fixture briefFixture, messageIDs map[string]int64, messageID int64) string {
	for _, message := range fixture.Messages {
		if messageIDs[message.Key] == messageID {
			return message.Key
		}
	}
	return fmt.Sprintf("unknown:%d", messageID)
}

func briefFixtureTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed.UTC()
}
