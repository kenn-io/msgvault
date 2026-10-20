//go:build sqlite_vec

package whoknows

import (
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// openPostgresFixtureBackend is set by backend_pg_test.go in pgvector builds.
var openPostgresFixtureBackend func(*testing.T, *store.Store) vector.Backend

const embedDimension = 512

// bagOfWordsEmbedder maps each word to a hashed dimension and normalizes, so
// similarity grows with shared words. It skips question filler, as a meaning
// model would, so "who knows about X" lands near messages about X. It is
// deterministic, and it proves behavior at the default floor; it does not
// calibrate a real model.
type bagOfWordsEmbedder struct{}

func (bagOfWordsEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return embedWords(text), nil
}

var fillerWords = map[string]bool{
	"who": true, "knows": true, "about": true, "the": true, "a": true, "an": true,
}

func embedWords(text string) []float32 {
	v := make([]float32, embedDimension)
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if fillerWords[word] {
			continue
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(word))
		v[h.Sum32()%embedDimension]++
	}
	var norm float64
	for _, x := range v {
		norm += float64(x * x)
	}
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(norm))
	}
	return v
}

type evalMailbox struct {
	Owner  string `json:"owner"`
	People []struct {
		Email      string   `json:"email"`
		Name       string   `json:"name"`
		Saved      bool     `json:"saved"`
		AskMeAbout []string `json:"ask_me_about"`
	} `json:"people"`
	Messages []struct {
		ID      string   `json:"id"`
		Date    string   `json:"date"`
		From    string   `json:"from"`
		To      []string `json:"to"`
		CC      []string `json:"cc"`
		Subject string   `json:"subject"`
		Body    string   `json:"body"`
		Type    string   `json:"type"`
		ListID  string   `json:"list_id"`
	} `json:"messages"`
}

type evalFixture struct {
	service *Service
	emails  map[int64]string // person ID to address
	engine  func(kPerSignal int) *hybrid.Engine
}

var evalNow = time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

func newEvalFixture(t *testing.T) *evalFixture {
	t.Helper()
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(t, err)
	raw, err := os.ReadFile("testdata/eval/mailbox.json")
	require.NoError(t, err)
	var box evalMailbox
	require.NoError(t, json.Unmarshal(raw, &box))
	require.NoError(t, st.AddAccountIdentity(source.ID, box.Owner, "manual"))

	participants := map[string]int64{}
	ensure := func(email string) int64 {
		if id, ok := participants[email]; ok {
			return id
		}
		id, err := st.EnsureParticipant(email, email, "example.net")
		require.NoError(t, err)
		participants[email] = id
		return id
	}
	emails := map[int64]string{}
	for _, p := range box.People {
		id, err := st.EnsureParticipant(p.Email, p.Name, "example.net")
		require.NoError(t, err)
		participants[p.Email] = id
		if !p.Saved {
			continue
		}
		person, _, err := st.CreatePersonFromParticipant(id)
		require.NoError(t, err)
		emails[person.ID] = p.Email
		for i, topic := range p.AskMeAbout {
			_, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
				PersonID: person.ID, DefinitionSlug: store.AttributeSlugAskMeAbout, Ordinal: new(int64(i)),
				Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &topic},
				Source: store.ProvenanceUser,
			})
			require.NoError(t, err)
		}
	}

	var backend vector.Backend
	if st.IsPostgreSQL() {
		if openPostgresFixtureBackend == nil {
			t.Skip("the PostgreSQL vector fixture needs the pgvector build tag")
		}
		backend = openPostgresFixtureBackend(t, st)
	} else {
		var seq int
		var name, mainPath string
		require.NoError(t, st.DB().QueryRow("PRAGMA database_list").Scan(&seq, &name, &mainPath))
		b, err := sqlitevec.Open(ctx, sqlitevec.Options{
			Path: filepath.Join(t.TempDir(), "vectors.db"), MainPath: mainPath,
			MainDB: st.DB(), Dimension: embedDimension,
		})
		require.NoError(t, err)
		backend = b
	}
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	generation, err := backend.CreateGeneration(ctx, "bag-of-words", embedDimension, "")
	require.NoError(t, err)

	conversation, err := st.EnsureConversation(source.ID, "evaluation", "Evaluation")
	require.NoError(t, err)
	var chunks []vector.Chunk
	for _, m := range box.Messages {
		sentAt, err := time.Parse(time.RFC3339, m.Date)
		require.NoError(t, err)
		id, err := st.UpsertMessage(&store.Message{
			ConversationID: conversation, SourceID: source.ID, SourceMessageID: m.ID,
			MessageType: m.Type, SenderID: sql.NullInt64{Int64: ensure(m.From), Valid: true},
			SentAt:  sql.NullTime{Time: sentAt, Valid: true},
			Subject: sql.NullString{String: m.Subject, Valid: true},
			Snippet: sql.NullString{String: m.Body, Valid: true},
			ListID:  sql.NullString{String: m.ListID, Valid: m.ListID != ""},
		})
		require.NoError(t, err)
		require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: m.Body, Valid: true}, sql.NullString{}))
		require.NoError(t, st.UpsertFTS(id, m.Subject, m.Body, m.From,
			strings.Join(m.To, ","), strings.Join(m.CC, ",")))
		for role, addresses := range map[string][]string{"to": m.To, "cc": m.CC} {
			var ids []int64
			for _, address := range addresses {
				ids = append(ids, ensure(address))
			}
			require.NoError(t, st.ReplaceMessageRecipients(id, role, ids, addresses))
		}
		chunks = append(chunks, vector.Chunk{
			MessageID: id, Vector: embedWords(m.Subject + " " + m.Body), SourceCharLen: len(m.Body),
		})
	}
	require.NoError(t, backend.Upsert(ctx, generation, chunks))
	require.NoError(t, backend.ActivateGeneration(ctx, generation, true))

	engine := func(kPerSignal int) *hybrid.Engine {
		return hybrid.NewEngine(backend, st.DB(), bagOfWordsEmbedder{},
			hybrid.Config{KPerSignal: kPerSignal, RRFK: 60, SubjectBoost: 1, Rebind: st.Rebind})
	}
	scorer, ok := backend.(vector.ChunkScoringBackend)
	require.True(t, ok, "the vector backend scores message chunks")
	return &evalFixture{
		service: &Service{
			Retriever: engine(100), Scorer: scorer, Data: st,
			Now: func() time.Time { return evalNow },
		},
		engine: engine,
		emails: emails,
	}
}

func (f *evalFixture) ranked(response Response) []string {
	var out []string
	for _, p := range response.People {
		out = append(out, f.emails[p.PersonID])
	}
	return out
}

// TestFindEvaluation runs the real store, vector backend, and hybrid engine at
// default settings. Topics nobody wrote about must return nothing: the
// nearest messages of an unrelated topic sit below the default floor.
func TestFindEvaluation(t *testing.T) {
	f := newEvalFixture(t)
	topics, stats, err := eval.LoadTopics("testdata/eval/topics.tsv")
	require.NoError(t, err)
	require.Equal(t, 8, stats.Parsed)
	qrels, _, err := eval.LoadQrels("testdata/eval/qrels.txt")
	require.NoError(t, err)

	for _, topic := range topics {
		t.Run(topic.ID, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			got, err := f.service.Find(t.Context(), Request{Topic: topic.Query})
			require.NoError(err)
			ranked := f.ranked(got)
			relevant := qrels.RelevantSet(topic.ID)

			if len(relevant) == 0 {
				assert.Empty(got.People, "judged: nobody wrote about it")
				assert.Empty(got.UnsavedContacts)
				assert.Equal(ReasonNoEvidence, got.Reason)
				return
			}
			assert.Empty(got.Reason)
			scores := eval.Evaluate(ranked, relevant, eval.CutoffsForDepth(10))
			assert.InDelta(1.0, scores.MRR, 1e-9, "the author ranks first: %v", ranked)
			assert.InDelta(1.0, scores.NDCG, 1e-9, "relevant people are listed in order: %v", ranked)
			assert.InDelta(1.0, scores.Recall, 1e-9, "every author is listed: %v", ranked)
			for _, p := range got.People {
				assert.Equal(p.AuthoredMessages == 0, len(p.Citations) == 0)
			}
			switch topic.ID {
			case "networking":
				assert.Equal([]string{"ana@example.net", "dee@example.net", "lurker@example.net"}, ranked,
					"authors first; a declared topic with no messages ranks on its boost")
				require.Len(got.UnsavedContacts, 1)
				assert.Equal("ben@example.net", got.UnsavedContacts[0].EmailAddress)
				assert.Len(got.People[0].Citations, 3)
				assert.Equal(5, got.People[0].AuthoredMessages)
				assert.Equal([]string{"kubernetes"}, got.People[1].AskMeAbout)
				assert.Zero(got.People[2].AuthoredMessages)
			case "decay":
				assert.Equal([]string{"recent@example.net", "old@example.net"}, ranked,
					"recent authors rank above old ones")
			case "ambiguous":
				assert.Equal([]string{"gopher@example.net"}, ranked,
					"keyword-only matches on good, going, and google do not count")
			}
		})
	}
}

// With a small candidate pool per signal, the keyword leg returns messages the
// meaning leg missed. Those hits carry no similarity and must not count.
func TestFindKeywordOnlyHitsDoNotCount(t *testing.T) {
	f := newEvalFixture(t)
	engine := f.engine(3)
	hits, _, err := engine.Search(t.Context(), hybrid.SearchRequest{
		Mode: hybrid.ModeHybrid, FreeText: "go", Limit: 200, SubjectTerms: []string{"go"},
	})
	require.NoError(t, err)
	keywordOnly := 0
	for _, h := range hits {
		if math.IsNaN(h.VectorScore) && !math.IsNaN(h.BM25Score) {
			keywordOnly++
		}
	}
	require.Positive(t, keywordOnly, "good, going, and google match the keyword leg")

	service := *f.service
	service.Retriever = engine
	got, err := service.Find(t.Context(), Request{Topic: "go"})
	require.NoError(t, err)
	assert.Equal(t, []string{"gopher@example.net"}, f.ranked(got))
}
