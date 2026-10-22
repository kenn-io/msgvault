//go:build sqlite_vec

package whoknows

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

var openPostgresFixtureBackend func(*testing.T, *store.Store) vector.Backend
var fixtureTopics = []string{"kubernetes networking", "certificate rotation", "C++ templates", "go", "capacity planning", "deployment automation", "unrelated geology", "owner dominated pool", "dated fallback"}

type fixtureEmbedder struct{ calls int }

func (e *fixtureEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	e.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return fixtureVector(text), nil
}
func fixtureVector(topic string) []float32 {
	v := make([]float32, len(fixtureTopics))
	for i, s := range fixtureTopics {
		if s == topic {
			v[i] = 1
			return v
		}
	}
	v[7] = 1
	return v
}

type mailboxFixture struct {
	People []struct {
		Email      string   `json:"email"`
		Name       string   `json:"name"`
		AskMeAbout []string `json:"ask_me_about"`
	} `json:"people"`
	Messages []struct {
		MessageID      string   `json:"message_id"`
		ConversationID string   `json:"conversation_id"`
		Date           string   `json:"date"`
		ReceivedDate   string   `json:"received_date"`
		InternalDate   string   `json:"internal_date"`
		Topic          string   `json:"topic"`
		From           string   `json:"from"`
		Subject        string   `json:"subject"`
		Body           string   `json:"body"`
		To             []string `json:"to"`
		CC             []string `json:"cc"`
		MessageType    string   `json:"message_type"`
		ListID         string   `json:"list_id"`
	} `json:"messages"`
}

func realServiceFixture(t *testing.T) (*Service, map[int64]string, *fixtureEmbedder) {
	t.Helper()
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(source.ID, "owner@example.net", "manual"))
	data, err := os.ReadFile("testdata/mailbox.json")
	require.NoError(t, err)
	var box mailboxFixture
	require.NoError(t, json.Unmarshal(data, &box))
	participants := map[string]int64{}
	persons := map[int64]string{}
	ensure := func(email string) int64 {
		if id, ok := participants[email]; ok {
			return id
		}
		id, err := st.EnsureParticipant(email, email, "example.net")
		require.NoError(t, err)
		participants[email] = id
		return id
	}
	for _, p := range box.People {
		id, err := st.EnsureParticipant(p.Email, p.Name, "example.net")
		require.NoError(t, err)
		participants[p.Email] = id
		person, _, err := st.CreatePersonFromParticipant(id)
		require.NoError(t, err)
		persons[person.ID] = p.Email
		for i, topic := range p.AskMeAbout {
			_, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugAskMeAbout, Ordinal: new(int64(i)), Value: store.AttributeValue{Type: store.AttributeValueText, Text: new(topic)}, Source: store.ProvenanceUser})
			require.NoError(t, err)
		}
	}
	parseFixtureTime := func(raw string) (sql.NullTime, error) {
		if raw == "" {
			return sql.NullTime{}, nil
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return sql.NullTime{}, fmt.Errorf("parse fixture time %q: %w", raw, err)
		}
		return sql.NullTime{Time: at, Valid: true}, nil
	}
	var backend vector.Backend
	if st.IsPostgreSQL() {
		if openPostgresFixtureBackend == nil {
			t.Skip("real PostgreSQL vector fixture needs pgvector build tag")
		}
		backend = openPostgresFixtureBackend(t, st)
	} else {
		var seq int
		var name, mainPath string
		require.NoError(t, st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &mainPath))
		b, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: mainPath, MainDB: st.DB(), Dimension: len(fixtureTopics)})
		require.NoError(t, err)
		backend = b
	}
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	gen, err := backend.CreateGeneration(ctx, "fixture", len(fixtureTopics), "")
	require.NoError(t, err)
	chunks := []vector.Chunk{}
	for _, m := range box.Messages {
		conv, err := st.EnsureConversation(source.ID, m.ConversationID, m.Subject)
		require.NoError(t, err)
		sentAt, err := parseFixtureTime(m.Date)
		require.NoError(t, err)
		receivedAt, err := parseFixtureTime(m.ReceivedDate)
		require.NoError(t, err)
		internalDate, err := parseFixtureTime(m.InternalDate)
		require.NoError(t, err)
		sender := ensure(m.From)
		id, err := st.UpsertMessage(&store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: m.MessageID, MessageType: m.MessageType, SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sentAt, ReceivedAt: receivedAt, InternalDate: internalDate, Subject: sql.NullString{String: m.Subject, Valid: true}, Snippet: sql.NullString{String: m.Body, Valid: true}, ListID: sql.NullString{String: m.ListID, Valid: m.ListID != ""}})
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: m.Body, Valid: true}, sql.NullString{}))
		for role, emails := range map[string][]string{"to": m.To, "cc": m.CC} {
			ids := []int64{}
			for _, email := range emails {
				ids = append(ids, ensure(email))
			}
			require.NoError(t, st.ReplaceMessageRecipients(id, role, ids, emails))
		}
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: fixtureVector(m.Topic), SourceCharLen: len(m.Body)})
	}
	meeting, err := meetingarchive.New(st).Upsert(ctx, meetingarchive.Snapshot{SourceID: source.ID, SourceMessageID: "meeting", SourceConversationID: "meeting", Title: "capacity planning", StartedAt: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), Body: "Synthetic capacity planning discussion.", Organizer: &meetingarchive.Person{Name: "Archive Owner", Email: "owner@example.net"}, Attendees: []meetingarchive.Person{{Name: "Attendee Example", Email: "attendee@example.net"}}}, meetingarchive.UpsertOptions{})
	require.NoError(t, err)
	chunks = append(chunks, vector.Chunk{MessageID: meeting.MessageID, Vector: fixtureVector("capacity planning"), SourceCharLen: 40})
	// Direct-chat recipients are inferred from the real roster, without recipient rows.
	chat, err := st.EnsureConversationWithType(source.ID, "direct-chat", "direct_chat", "kubernetes networking")
	require.NoError(t, err)
	for _, email := range []string{"owner@example.net", "received@example.net"} {
		require.NoError(t, st.EnsureConversationParticipant(chat, ensure(email), "member"))
	}
	chatID, err := st.UpsertMessage(&store.Message{ConversationID: chat, SourceID: source.ID, SourceMessageID: "chat", MessageType: "chat", SenderID: sql.NullInt64{Int64: ensure("owner@example.net"), Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), Valid: true}, Subject: sql.NullString{String: "kubernetes networking", Valid: true}})
	require.NoError(t, err)
	chunks = append(chunks, vector.Chunk{MessageID: chatID, Vector: fixtureVector("kubernetes networking"), SourceCharLen: 30})
	large, err := st.EnsureConversationWithType(source.ID, "large-room", "group_chat", "Large room")
	require.NoError(t, err)
	for i := range store.MaxRosterEdges + 1 {
		require.NoError(t, st.EnsureConversationParticipant(large, ensure(fmt.Sprintf("room%d@example.net", i)), "member"))
	}
	largeID, err := st.UpsertMessage(&store.Message{ConversationID: large, SourceID: source.ID, SourceMessageID: "large-room", MessageType: "chat", SenderID: sql.NullInt64{Int64: ensure("owner@example.net"), Valid: true}, Subject: sql.NullString{String: "kubernetes networking", Valid: true}})
	require.NoError(t, err)
	chunks = append(chunks, vector.Chunk{MessageID: largeID, Vector: fixtureVector("kubernetes networking"), SourceCharLen: 30})
	require.NoError(t, backend.Upsert(ctx, gen, chunks))
	require.NoError(t, backend.ActivateGeneration(ctx, gen, true))
	embedder := &fixtureEmbedder{}
	engine := hybrid.NewEngine(backend, st.DB(), embedder, hybrid.Config{KPerSignal: 100, RRFK: 60, SubjectBoost: 1, Rebind: st.Rebind})
	s := &Service{Retriever: engine, Data: st, KPerSignal: 100, Now: func() time.Time { return time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC) }, CoverageReader: func(context.Context) (vector.EmbeddingStatus, error) {
		return vector.EmbeddingStatus{Generation: vector.EmbeddingGenerationStatus{ID: gen, State: string(vector.GenerationActive)}, Eligible: int64(len(chunks)), Current: int64(len(chunks)), CoverageSampledAt: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}, nil
	}}
	return s, persons, embedder
}

func TestWhoKnowsGradedRetrieval(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	s, names, embedder := realServiceFixture(t)
	topics, stats, err := eval.LoadTopics("testdata/eval/topics.tsv")
	must.NoError(err)
	check.Equal(8, stats.Parsed)
	qrels, _, err := eval.LoadQrels("testdata/eval/qrels.txt")
	must.NoError(err)
	for _, topic := range topics {
		t.Run(topic.ID, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			p := DefaultParams()
			p.MinScore = .8
			before := embedder.calls
			got, err := s.Find(t.Context(), topic.Query, p)
			must.NoError(err)
			check.Equal(before+1, embedder.calls)
			ranked := []string{}
			for _, r := range got.People {
				ranked = append(ranked, names[r.PersonID])
				for _, e := range r.Evidence {
					check.Positive(e.MessageID)
					check.NotNil(e.SentAt)
				}
				b := r.Breakdown
				check.InDelta(r.Score, b.Evidence+b.Declared+b.Affinity, 1e-9)
				check.InDelta(b.RawAuthored+b.RawReceived, b.ShownContribution+b.OmittedContribution, 1e-9)
			}
			if len(qrels.RelevantSet(topic.ID)) == 0 {
				check.Empty(ranked)
				check.NotNil(got.Reason)
			} else {
				check.GreaterOrEqual(eval.GradedNDCGAt(ranked, qrels[topic.ID], 5), .95, ranked)
			}
			switch topic.ID {
			case "networking":
				must.GreaterOrEqual(len(ranked), 3)
				check.Equal([]string{"author@example.net", "declared@example.net"}, ranked[:2])
				check.Positive(got.Excluded.OwnerPeople)
				check.Positive(got.Excluded.AutomatedSenders)
				check.Positive(got.Excluded.LargeRooms)
				check.NotEmpty(got.UnsavedCandidates)
			case "decay":
				check.Equal([]string{"recent@example.net", "old@example.net"}, ranked)
			case "collision":
				check.Equal([]string{"go@example.net"}, ranked)
			case "meeting":
				must.Len(got.People, 1)
				check.Zero(got.People[0].Breakdown.AuthoredMessages)
				check.Equal("conversation_member", got.People[0].Evidence[0].Role)
			case "exclusions":
				check.Positive(got.Excluded.MessageTypes)
			case "starvation":
				check.True(got.Retrieval.Underfilled)
			}
		})
	}
	pMeeting := DefaultParams()
	pMeeting.IncludeOwner = true
	pMeeting.MinScore = .8
	meetingResult, err := s.Find(t.Context(), "capacity planning", pMeeting)
	must.NoError(err)
	must.Len(meetingResult.People, 2)
	for _, person := range meetingResult.People {
		check.Zero(person.Breakdown.RawAuthored)
		for _, e := range person.Evidence {
			check.NotContains(e.Flags, "owner_authored")
		}
	}
	p := DefaultParams()
	p.MinScore = .8
	after := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	before := after.Add(24 * time.Hour)
	p.After = &after
	p.Before = &before
	got, err := s.Find(t.Context(), "certificate rotation", p)
	must.NoError(err)
	must.Len(got.People, 1)
	check.Equal("recent@example.net", names[got.People[0].PersonID])
	p.After = &before
	p.Before = new(before.Add(time.Hour))
	got, err = s.Find(t.Context(), "certificate rotation", p)
	must.NoError(err)
	check.Empty(got.People)
}

func TestWhoKnowsDateFilterUsesReceivedAndInternalDateFallback(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	s, names, _ := realServiceFixture(t)
	after := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	before := after.Add(24 * time.Hour)
	p := DefaultParams()
	p.MinScore = .8
	p.After = &after
	p.Before = &before

	got, err := s.Find(t.Context(), "dated fallback", p)
	must.NoError(err)
	must.Len(got.People, 2)
	gotDates := map[string]time.Time{}
	for _, person := range got.People {
		for _, evidence := range person.Evidence {
			must.NotNil(evidence.SentAt)
			gotDates[names[person.PersonID]] = *evidence.SentAt
		}
	}
	check.Equal(after.Add(12*time.Hour), gotDates["author@example.net"])
	check.Equal(after.Add(13*time.Hour), gotDates["declared@example.net"])
}
