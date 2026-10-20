//go:build sqlite_vec

package whoknows

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"slices"
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

// scaledEmbedder embeds as bagOfWordsEmbedder does, then scales the vector,
// as a provider that does not normalize its vectors would.
type scaledEmbedder struct{ scale float32 }

func (e scaledEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return scaled(embedWords(text), e.scale), nil
}

func scaled(v []float32, by float32) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * by
	}
	return out
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
	service      *Service
	emails       map[int64]string // person ID to address
	store        *store.Store
	backend      vector.Backend
	generation   vector.GenerationID
	source       int64
	conversation int64
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
	// The embedding worker stamps each message it embeds.
	_, err = st.DB().ExecContext(ctx, st.Rebind("UPDATE messages SET embed_gen = ?"), int64(generation))
	require.NoError(t, err)

	coverage, ok := backend.(vector.FilteredCoverageBackend)
	require.True(t, ok, "backend must report filtered coverage")

	return &evalFixture{
		service: &Service{
			Retriever: evalEngine(st, backend, bagOfWordsEmbedder{}), Data: st, Index: coverage,
			Now: func() time.Time { return evalNow },
		},
		emails: emails, store: st, backend: backend,
		generation: generation, source: source.ID, conversation: conversation,
	}
}

func evalEngine(st *store.Store, backend vector.Backend, embedder hybrid.EmbeddingClient) *hybrid.Engine {
	return hybrid.NewEngine(backend, st.DB(), embedder,
		hybrid.Config{KPerSignal: 100, RRFK: 60, SubjectBoost: 1, Rebind: st.Rebind})
}

// post stores an email from a new sender, indexed for keyword search. Unless
// vec is nil, it also embeds the email as vec and stamps it, as the embedding
// worker does; mail synced since the last embedding run has neither.
func (f *evalFixture) post(t *testing.T, from, text string, vec []float32) int64 {
	t.Helper()
	sender, err := f.store.EnsureParticipant(from, from, "example.net")
	require.NoError(t, err)
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID: f.conversation, SourceID: f.source, SourceMessageID: from, MessageType: "email",
		SenderID: sql.NullInt64{Int64: sender, Valid: true},
		SentAt:   sql.NullTime{Time: evalNow, Valid: true},
		Subject:  sql.NullString{String: text, Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertFTS(id, text, text, from, "", ""))
	if vec != nil {
		require.NoError(t, f.backend.Upsert(t.Context(), f.generation,
			[]vector.Chunk{{MessageID: id, Vector: vec, SourceCharLen: len(text)}}))
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind("UPDATE messages SET embed_gen = ? WHERE id = ?"),
			int64(f.generation), id)
		require.NoError(t, err)
	}
	return id
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
				assert.Empty(got.Limits)
				return
			}
			assert.Empty(got.Limits)
			scores := eval.Evaluate(ranked, relevant, eval.CutoffsForDepth(10))
			assert.InDelta(1.0, scores.MRR, 1e-9, "the author ranks first: %v", ranked)
			assert.InDelta(1.0, scores.NDCG, 1e-9, "relevant people are listed in order: %v", ranked)
			assert.InDelta(1.0, scores.Recall, 1e-9, "every author is listed: %v", ranked)
			for _, p := range got.People {
				assert.Equal(p.AuthoredMessages == 0, len(p.Citations) == 0)
			}
			if topic.ID == "ambiguous" {
				assert.Equal([]string{"gopher@example.net"}, ranked,
					"keyword-only matches on good, going, and google do not count")
			}
		})
	}
}

// Mail synced since the last embedding run is found only by keyword, which
// does not count, so the result says so rather than that nobody wrote.
func TestFindReportsMessagesNotYetEmbedded(t *testing.T) {
	f := newEvalFixture(t)
	f.post(t, "fresh@example.net", "zeppelin mooring lines", nil)

	got, err := f.service.Find(t.Context(), Request{Topic: "zeppelin mooring"})
	require.NoError(t, err)
	assert.Empty(t, got.UnsavedContacts)
	assert.Equal(t, []string{LimitNotEmbedded}, got.Limits)
}

// Search scores a vector hit as 1 - L2 distance, which falls below the floor
// for a message sharing two of three words with the topic. The service
// rescores it by cosine.
func TestFindCountsVectorHitsByCosine(t *testing.T) {
	f := newEvalFixture(t)
	f.post(t, "captain@example.net", "zeppelin mooring lines", embedWords("zeppelin mooring lines"))

	got, err := f.service.Find(t.Context(), Request{Topic: "zeppelin mooring"})
	require.NoError(t, err)
	require.Len(t, got.UnsavedContacts, 1)
	assert.Equal(t, "captain@example.net", got.UnsavedContacts[0].EmailAddress)
	// Two of three words shared: cosine 2/sqrt(6), sent today.
	assert.InDelta(t, math.Log1p(2/math.Sqrt(6)), got.UnsavedContacts[0].MessageScore, 1e-6)
}

// sqlite-vec ranks the pool by L2 distance, which follows cosine order only
// for unit-length vectors. With a query that is not unit length, a full pool
// where nothing reached the floor can hide a message that would, so the
// result says the pool was full rather than that nobody wrote.
func TestFindReportsAFullL2PoolForAQueryThatIsNotUnitLength(t *testing.T) {
	f := newEvalFixture(t)
	if f.store.IsPostgreSQL() {
		t.Skip("pgvector ranks by cosine distance")
	}
	topic := embedWords("zeppelin mooring")
	// Each filler is 3 units long, at cosine 0.3 to the topic: below the
	// floor, but nearer the query by L2 than the answer below.
	off := slices.IndexFunc(topic, func(x float32) bool { return x == 0 })
	filler := scaled(topic, .3)
	filler[off] = float32(math.Sqrt(1 - .3*.3))
	filler = scaled(filler, 3)
	for i := range 101 {
		f.post(t, fmt.Sprintf("filler%d@example.net", i), "routine note", filler)
	}

	got, err := f.service.Find(t.Context(), Request{Topic: "zeppelin mooring"})
	require.NoError(t, err)
	assert.Empty(t, got.UnsavedContacts)
	assert.Empty(t, got.Limits, "a unit-length query over a pool below the floor is an unrelated topic")

	// The answer points the topic's way but is 100 units long, so it sits
	// past every filler by L2 and outside the pool.
	f.post(t, "captain@example.net", "airship tethering", scaled(topic, 100))
	f.service.Retriever = evalEngine(f.store, f.backend, scaledEmbedder{3})
	got, err = f.service.Find(t.Context(), Request{Topic: "zeppelin mooring"})
	require.NoError(t, err)
	assert.Empty(t, got.UnsavedContacts)
	assert.Equal(t, []string{LimitPoolFull}, got.Limits)
}
