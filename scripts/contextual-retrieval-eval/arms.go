//go:build sqlite_vec

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

type HTTPAttemptSnapshot struct {
	Attempts            int64          `json:"attempts"`
	Statuses            map[int]int64  `json:"statuses"`
	TransportErrors     int64          `json:"transport_errors"`
	SuccessfulResponses int64          `json:"successful_responses"`
	ResponseTokens      int64          `json:"response_tokens"`
	ResponseDocuments   int64          `json:"response_documents"`
	UsageResponses      int64          `json:"usage_responses"`
	ErrorMessages       []string       `json:"error_messages,omitempty"`
	LatencyMillis       LatencySummary `json:"latency_ms"`
	latencySamples      []time.Duration
}

type replayMutation struct {
	Day             int       `json:"day"`
	MessageID       int64     `json:"message_id"`
	SourceMessageID string    `json:"source_message_id"`
	SentAt          time.Time `json:"sent_at"`
	ContentHash     string    `json:"content_hash"`
}

type replayTick struct {
	Day            int `json:"day"`
	AfterMutations int `json:"after_mutations"`
}

type replayPathStats struct {
	Publications                int            `json:"publications"`
	Replacements                int            `json:"replacements"`
	EmbeddedDocuments           int            `json:"embedded_documents"`
	ProviderTokens              int64          `json:"provider_tokens"`
	ProviderRequests            int64          `json:"provider_requests"`
	ProviderSuccessfulResponses int64          `json:"provider_successful_responses"`
	ProviderUsageResponses      int64          `json:"provider_usage_responses"`
	ProviderErrors              int64          `json:"provider_errors"`
	ProviderStatuses            map[int]int64  `json:"provider_statuses,omitempty"`
	UsageAvailable              bool           `json:"usage_available"`
	LatencyMillis               LatencySummary `json:"latency_ms"`
	Error                       string         `json:"error,omitempty"`
}

type productionReplayHistory struct {
	Path                      string           `json:"path"`
	Mutations                 []replayMutation `json:"mutations"`
	Ticks                     []replayTick     `json:"ticks"`
	LegacyScheduled           replayPathStats  `json:"legacy_scheduled"`
	ContextualScheduled       replayPathStats  `json:"contextual_scheduled"`
	ContextualPerAppendStress replayPathStats  `json:"contextual_per_append_stress"`
}

type measuredChildResult struct {
	Output            []byte
	Stderr            []byte
	MaxRSSBytes       *int64
	MemoryMeasurement string
	ExitError         string
}

func runMeasuredChildCommand(cmd *exec.Cmd) measuredChildResult {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := measuredChildResult{Output: stdout.Bytes(), Stderr: stderr.Bytes(), MemoryMeasurement: "unavailable"}
	if state := cmd.ProcessState; state != nil {
		result.MaxRSSBytes, result.MemoryMeasurement = childMaxRSS(state)
	}
	if err != nil {
		result.ExitError = err.Error()
	}
	return result
}

type replaySemanticClient struct {
	dimension    int
	documents    int
	documentDays []string
	attempts     int64
	tokens       int64
}

func (c *replaySemanticClient) EmbedQuery(context.Context, string) ([]float32, error) {
	return make([]float32, c.dimension), nil
}

func (c *replaySemanticClient) EmbedDocuments(_ context.Context, documents []embed.DocumentInput) ([][][]float32, error) {
	c.documents += len(documents)
	c.attempts++
	result := make([][][]float32, len(documents))
	for i, document := range documents {
		if len(document.Chunks) != 0 {
			if _, after, found := strings.Cut(document.Chunks[0], "Date: "); found {
				day, _, _ := strings.Cut(after, "\n")
				c.documentDays = append(c.documentDays, day)
			}
		}
		result[i] = make([][]float32, len(document.Chunks))
		for j := range document.Chunks {
			c.tokens += 7
			result[i][j] = make([]float32, c.dimension)
			result[i][j][0] = 1
		}
	}
	return result, nil
}

func (c *replaySemanticClient) replayDocumentDays() []string {
	return append([]string(nil), c.documentDays...)
}

func (c *replaySemanticClient) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	c.documents += len(inputs)
	c.attempts++
	c.tokens += int64(len(inputs)) * 5
	result := make([][]float32, len(inputs))
	for i := range inputs {
		result[i] = make([]float32, c.dimension)
		result[i][0] = 1
	}
	return result, nil
}

func (c *replaySemanticClient) replayUsage() HTTPAttemptSnapshot {
	latencies := make([]time.Duration, c.attempts)
	for i := range latencies {
		latencies[i] = time.Millisecond
	}
	return HTTPAttemptSnapshot{Attempts: c.attempts, SuccessfulResponses: c.attempts,
		UsageResponses: c.attempts, ResponseTokens: c.tokens, ResponseDocuments: int64(c.documents),
		Statuses:      map[int]int64{http.StatusOK: c.attempts},
		LatencyMillis: summarizeLatencies(latencies), latencySamples: latencies}
}

type replayEventKind string

const (
	replayMutationEvent replayEventKind = "mutation"
	replayTickEvent     replayEventKind = "tick"
)

type replayEvent struct {
	Kind            replayEventKind
	Day             int
	SourceMessageID string
	SentAt          time.Time
	Body            string
}

type replayRuneDistribution struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type replayWorkload struct {
	Mutations        int                    `json:"mutations"`
	ScheduledTicks   int                    `json:"scheduled_ticks"`
	BodyRunes        replayRuneDistribution `json:"body_runes"`
	CapacityForecast bool                   `json:"capacity_forecast"`
	Interpretation   string                 `json:"interpretation"`
}

type replayClients struct {
	LegacyScheduled           embed.EmbeddingClient
	ContextualScheduled       embed.SemanticClient
	ContextualPerAppendStress embed.SemanticClient
	Observer                  *httpAttemptObserver
}

func buildReplayEvents(days int) []replayEvent {
	events := make([]replayEvent, 0, days*3)
	for day := range days {
		date := time.Date(2030, 1, day+1, 9, 0, 0, 0, time.UTC)
		for sequence := range 2 {
			events = append(events, replayEvent{Kind: replayMutationEvent, Day: day + 1,
				SourceMessageID: fmt.Sprintf("day-%02d-append-%02d", day+1, sequence+1),
				SentAt:          date.Add(time.Duration(sequence) * time.Minute),
				Body:            replayBody(day+1, sequence+1)})
		}
		events = append(events, replayEvent{Kind: replayTickEvent, Day: day + 1})
	}
	return events
}

func replayBody(day, sequence int) string {
	sentences := []string{
		"The project group reviewed the current status, confirmed the next action, and recorded an owner for follow-up.",
		"The participants compared two alternatives, noted an unresolved dependency, and agreed to verify the result at the next review.",
		"A short implementation note described the expected behavior, the affected users, and the evidence needed before release.",
		"The discussion also captured a fallback, a clear decision boundary, and the condition that would require another sync.",
	}
	paragraphs := []int{1, 2, 4, 6, 3, 8, 5}
	count := paragraphs[(day+sequence-2)%len(paragraphs)]
	parts := make([]string, 0, count+1)
	parts = append(parts, fmt.Sprintf("Synthetic replay day %d append %d.", day, sequence))
	for i := range count {
		parts = append(parts, sentences[(day+sequence+i-2)%len(sentences)])
	}
	return strings.Join(parts, " ")
}

func summarizeReplayWorkload(events []replayEvent) replayWorkload {
	result := replayWorkload{
		Interpretation: "deterministic scrubbed regression; not a production capacity forecast",
	}
	for _, event := range events {
		switch event.Kind {
		case replayMutationEvent:
			length := len([]rune(event.Body))
			if result.Mutations == 0 || length < result.BodyRunes.Min {
				result.BodyRunes.Min = length
			}
			if length > result.BodyRunes.Max {
				result.BodyRunes.Max = length
			}
			result.Mutations++
		case replayTickEvent:
			result.ScheduledTicks++
		}
	}
	return result
}

func replayProductionHistory(ctx context.Context, parent string, days int) (productionReplayHistory, error) {
	if days <= 0 {
		return productionReplayHistory{}, errors.New("replay days must be positive")
	}
	legacy := &replaySemanticClient{dimension: evaluationDimension}
	eager := &replaySemanticClient{dimension: evaluationDimension}
	delayed := &replaySemanticClient{dimension: evaluationDimension}
	return replayProductionHistoryWithClients(ctx, parent, buildReplayEvents(days), replayClients{
		LegacyScheduled: legacy, ContextualScheduled: eager, ContextualPerAppendStress: delayed})
}

func replayProductionHistoryWithClients(ctx context.Context, parent string, events []replayEvent, clients replayClients) (productionReplayHistory, error) {
	if len(events) == 0 {
		return productionReplayHistory{}, errors.New("replay events must not be empty")
	}
	legacy, legacyTrace, err := runReplayMode(ctx, parent, events, "legacy_scheduled", clients.LegacyScheduled, clients.Observer)
	if err != nil {
		return productionReplayHistory{}, err
	}
	scheduled, scheduledTrace, err := runReplayMode(ctx, parent, events, "contextual_scheduled", clients.ContextualScheduled, clients.Observer)
	if err != nil {
		return productionReplayHistory{}, err
	}
	stress, stressTrace, err := runReplayMode(ctx, parent, events, "contextual_per_append_stress", clients.ContextualPerAppendStress, clients.Observer)
	stressAvailable := err == nil
	if !stressAvailable {
		stress = replayPathStats{Error: "per-append stress diagnostic failed"}
	}
	if !slices.Equal(legacyTrace.MessageIDs, scheduledTrace.MessageIDs) ||
		(stressAvailable && !slices.Equal(legacyTrace.MessageIDs, stressTrace.MessageIDs)) {
		return productionReplayHistory{}, errors.New("replay modes did not apply identical message identities")
	}
	if !slices.Equal(legacyTrace.Ticks, scheduledTrace.Ticks) ||
		(stressAvailable && !slices.Equal(legacyTrace.Ticks, stressTrace.Ticks)) {
		return productionReplayHistory{}, errors.New("replay modes did not apply identical tick boundaries")
	}
	mutations := make([]replayMutation, 0, len(legacyTrace.MessageIDs))
	mutationIndex := 0
	for _, event := range events {
		if event.Kind != replayMutationEvent {
			continue
		}
		mutations = append(mutations, replayMutation{Day: event.Day, MessageID: legacyTrace.MessageIDs[mutationIndex], SourceMessageID: event.SourceMessageID,
			SentAt: event.SentAt, ContentHash: fmt.Sprintf("%x", sha256.Sum256([]byte(event.Body)))})
		mutationIndex++
	}
	return productionReplayHistory{
		Path:                      "shared_mutations_and_production_ticks_worker_context_workers_publication_ledger",
		Mutations:                 mutations,
		Ticks:                     legacyTrace.Ticks,
		LegacyScheduled:           legacy,
		ContextualScheduled:       scheduled,
		ContextualPerAppendStress: stress,
	}, nil
}

type replayTrace struct {
	MessageIDs []int64
	Ticks      []replayTick
}

func runReplayMode(ctx context.Context, parent string, events []replayEvent, mode string, client any,
	observer *httpAttemptObserver) (replayPathStats, replayTrace, error) {
	dir, err := os.MkdirTemp(parent, "production-replay-")
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	mainPath := filepath.Join(dir, "main.db")
	st, err := store.OpenForTest(mainPath)
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	defer func() { _ = st.Close() }()
	if err := st.InitSchema(); err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: filepath.Join(dir, "vectors.db"),
		MainDB: st.DB(), MainPath: mainPath, Dimension: evaluationDimension})
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	defer func() { _ = backend.Close() }()
	model := context4Model
	if mode == "legacy_scheduled" {
		model = oldProductionModel
	}
	generation, err := backend.CreateGeneration(ctx, model, evaluationDimension, "production-replay:"+mode)
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	source, err := st.GetOrCreateSource("contextual_eval", "shared-replay-stream")
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	conversation, err := st.EnsureConversationWithType(source.ID, "replay-chat", "beeper", "Synthetic chat")
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	participant, err := st.EnsureParticipant("synthetic-replay", "Synthetic Speaker", "")
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	if err := st.EnsureConversationParticipant(conversation, participant, "member"); err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	policy := evaluationAssemblyPolicy()
	var runOnce func(context.Context, vector.GenerationID, operations.PassScope) (embed.RunResult, error)
	if mode == "legacy_scheduled" {
		legacy, ok := client.(embed.EmbeddingClient)
		if !ok {
			return replayPathStats{}, replayTrace{}, errors.New("legacy replay requires embedding client")
		}
		worker := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: st.DB(), Store: st,
			Client: legacy, Preprocess: productionPreprocessConfig(), MaxInputChars: evaluationChunkRunes, BatchSize: 32,
			Recorder: st})
		runOnce = worker.RunOnce
	} else {
		semantic, ok := client.(embed.SemanticClient)
		if !ok {
			return replayPathStats{}, replayTrace{}, errors.New("contextual replay requires semantic client")
		}
		worker := embed.NewContextWorker(embed.ContextWorkerDeps{Backend: backend, Publisher: backend, Store: st,
			Assembler: embed.CompositeAssembler{Policy: policy, Chat: embed.ChatWindowAssembler{Policy: policy}},
			Client:    semantic, ChangeBatchSize: 64, ReconcileBatchSize: 128, Recorder: st})
		runOnce = worker.RunOnce
	}
	before := replayUsageSnapshot(client, observer)
	trace := replayTrace{MessageIDs: make([]int64, 0, len(events)), Ticks: make([]replayTick, 0, len(events)/3)}
	for _, event := range events {
		if event.Kind == replayTickEvent {
			trace.Ticks = append(trace.Ticks, replayTick{Day: event.Day, AfterMutations: len(trace.MessageIDs)})
			if mode == "contextual_per_append_stress" {
				continue
			}
			scope := operations.PassScope{
				Key:     fmt.Sprintf("contextual-eval:%s:tick:%d", mode, len(trace.Ticks)),
				Trigger: operations.TriggerManual, StartedAt: time.Now().UTC(),
			}
			_, err := runOnce(ctx, generation, scope)
			if err != nil {
				return replayPathStats{}, replayTrace{}, err
			}
			continue
		}
		if event.Kind != replayMutationEvent {
			return replayPathStats{}, replayTrace{}, fmt.Errorf("unknown replay event kind %q", event.Kind)
		}
		messageID, err := st.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: source.ID,
			SourceMessageID: event.SourceMessageID, MessageType: "beeper",
			SentAt:   sql.NullTime{Time: event.SentAt, Valid: true},
			SenderID: sql.NullInt64{Int64: participant, Valid: true}})
		if err != nil {
			return replayPathStats{}, replayTrace{}, err
		}
		if err := st.UpsertMessageBody(messageID, sql.NullString{String: event.Body, Valid: true}, sql.NullString{}); err != nil {
			return replayPathStats{}, replayTrace{}, err
		}
		trace.MessageIDs = append(trace.MessageIDs, messageID)
		if mode == "contextual_per_append_stress" {
			scope := evaluationPassScope(fmt.Sprintf(
				"contextual-eval:%s:append:%d", mode, len(trace.MessageIDs),
			))
			_, err := runOnce(ctx, generation, scope)
			if err != nil {
				return replayPathStats{}, replayTrace{}, err
			}
		}
	}
	records, err := backend.ListDocumentsAfter(ctx, generation, "", 1000)
	if err != nil {
		return replayPathStats{}, replayTrace{}, err
	}
	usage := httpAttemptDelta(before, replayUsageSnapshot(client, observer))
	embeddedDocuments := int(usage.ResponseDocuments)
	publications := embeddedDocuments
	replacements := 0
	if mode != "legacy_scheduled" {
		replacements = max(publications-len(records), 0)
	}
	return replayPathStats{Publications: publications, Replacements: replacements,
		EmbeddedDocuments: embeddedDocuments, ProviderTokens: usage.ResponseTokens,
		ProviderRequests: usage.Attempts, ProviderSuccessfulResponses: usage.SuccessfulResponses,
		ProviderUsageResponses: usage.UsageResponses, ProviderErrors: usage.ErrorCount(),
		ProviderStatuses: cloneHTTPStatuses(usage.Statuses), UsageAvailable: replayUsageAvailable(usage),
		LatencyMillis: usage.LatencyMillis}, trace, nil
}

func replayUsageSnapshot(client any, observer *httpAttemptObserver) HTTPAttemptSnapshot {
	if observer != nil {
		return observer.Snapshot()
	}
	if reporter, ok := client.(interface{ replayUsage() HTTPAttemptSnapshot }); ok {
		return reporter.replayUsage()
	}
	return HTTPAttemptSnapshot{Statuses: make(map[int]int64)}
}

type httpAttemptObserver struct {
	mu        sync.Mutex
	snapshot  HTTPAttemptSnapshot
	latencies []time.Duration
}

func (o *httpAttemptObserver) Snapshot() HTTPAttemptSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := o.snapshot
	result.Statuses = make(map[int]int64, len(o.snapshot.Statuses))
	maps.Copy(result.Statuses, o.snapshot.Statuses)
	result.ErrorMessages = append([]string(nil), o.snapshot.ErrorMessages...)
	result.latencySamples = append([]time.Duration(nil), o.latencies...)
	result.LatencyMillis = summarizeLatencies(result.latencySamples)
	return result
}

func httpAttemptDelta(before, after HTTPAttemptSnapshot) HTTPAttemptSnapshot {
	result := HTTPAttemptSnapshot{Attempts: after.Attempts - before.Attempts,
		TransportErrors:     after.TransportErrors - before.TransportErrors,
		SuccessfulResponses: after.SuccessfulResponses - before.SuccessfulResponses,
		ResponseTokens:      after.ResponseTokens - before.ResponseTokens,
		ResponseDocuments:   after.ResponseDocuments - before.ResponseDocuments,
		UsageResponses:      after.UsageResponses - before.UsageResponses,
		Statuses:            make(map[int]int64)}
	for status, count := range after.Statuses {
		result.Statuses[status] = count - before.Statuses[status]
	}
	if len(after.ErrorMessages) > len(before.ErrorMessages) {
		result.ErrorMessages = append([]string(nil), after.ErrorMessages[len(before.ErrorMessages):]...)
	}
	if len(after.latencySamples) > len(before.latencySamples) {
		result.latencySamples = append([]time.Duration(nil), after.latencySamples[len(before.latencySamples):]...)
		result.LatencyMillis = summarizeLatencies(result.latencySamples)
	}
	return result
}

func (s HTTPAttemptSnapshot) UsageAvailable() bool {
	return s.SuccessfulResponses > 0 && s.UsageResponses == s.SuccessfulResponses && s.ResponseTokens > 0
}

func replayUsageAvailable(s HTTPAttemptSnapshot) bool {
	return s.UsageAvailable() && s.ErrorCount() == 0 && s.Attempts == s.SuccessfulResponses &&
		s.ResponseDocuments > 0
}

func cloneHTTPStatuses(statuses map[int]int64) map[int]int64 {
	cloned := make(map[int]int64, len(statuses))
	maps.Copy(cloned, statuses)
	return cloned
}

func (s HTTPAttemptSnapshot) ErrorCount() int64 {
	total := s.TransportErrors
	for status, count := range s.Statuses {
		if status >= 400 {
			total += count
		}
	}
	return total
}

func newCountingEmbeddingProxy(upstream string) (http.Handler, *httpAttemptObserver, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, nil, fmt.Errorf("parse embedding upstream: %w", err)
	}
	observer := &httpAttemptObserver{snapshot: HTTPAttemptSnapshot{Statuses: make(map[int]int64)}}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = target.Host
		},
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			return fmt.Errorf("read embedding response body: %w", err)
		}
		response.Body = io.NopCloser(bytes.NewReader(payload))
		var envelope struct {
			Usage struct {
				TotalTokens int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(payload, &envelope)
		observer.mu.Lock()
		observer.snapshot.Statuses[response.StatusCode]++
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			observer.snapshot.SuccessfulResponses++
			if count, ok := response.Request.Context().Value(embeddingDocumentCountKey{}).(int64); ok {
				observer.snapshot.ResponseDocuments += count
			}
		}
		if envelope.Usage.TotalTokens > 0 && response.StatusCode >= 200 && response.StatusCode < 300 {
			observer.snapshot.ResponseTokens += envelope.Usage.TotalTokens
			observer.snapshot.UsageResponses++
		}
		observer.mu.Unlock()
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		observer.mu.Lock()
		observer.snapshot.TransportErrors++
		if len(observer.snapshot.ErrorMessages) < 20 {
			observer.snapshot.ErrorMessages = append(observer.snapshot.ErrorMessages, err.Error())
		}
		observer.mu.Unlock()
		http.Error(w, "embedding upstream unavailable", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		start := time.Now()
		documentCount := embeddingRequestDocumentCount(request)
		request = request.WithContext(context.WithValue(request.Context(), embeddingDocumentCountKey{}, documentCount))
		observer.mu.Lock()
		observer.snapshot.Attempts++
		observer.mu.Unlock()
		proxy.ServeHTTP(w, request)
		observer.mu.Lock()
		observer.latencies = append(observer.latencies, retainMeasuredLatency(time.Since(start)))
		observer.mu.Unlock()
	})
	return handler, observer, nil
}

func retainMeasuredLatency(value time.Duration) time.Duration {
	if value <= 0 {
		return time.Nanosecond
	}
	return value
}

type embeddingDocumentCountKey struct{}

func embeddingRequestDocumentCount(request *http.Request) int64 {
	payload, err := io.ReadAll(request.Body)
	request.Body = io.NopCloser(bytes.NewReader(payload))
	if err != nil {
		return 0
	}
	var envelope struct {
		Input  json.RawMessage `json:"input"`
		Inputs json.RawMessage `json:"inputs"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return 0
	}
	raw := envelope.Input
	if len(envelope.Inputs) != 0 {
		raw = envelope.Inputs
	}
	if len(raw) == 0 {
		return 0
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		return int64(len(items))
	}
	return 1
}

const (
	ArmOldProduction        = "O-prod"
	ArmOldContext4Singleton = "O-c4"
	ArmStructuredSingleton  = "S-c4"
	ArmNestedContext4       = "N-c4"

	oldProductionModel   = "voyage-4-large"
	context4Model        = "voyage-context-4"
	evaluationDimension  = 1024
	evaluationChunkRunes = 32768
	// evaluationDocumentMax must equal the production document byte cap
	// (contextualDocumentUTF8Limit in cmd/msgvault/cmd/serve_vector.go) so the
	// eval measures the windowing policy users actually get.
	evaluationDocumentMax = 100_000
)

type EvalChunk struct {
	ExternalID  string
	SourceID    string
	MessageID   int64
	ChunkIndex  int
	Text        string
	SourceStart int
	SourceEnd   int
	SourceBasis vector.SourceBasis
}

type ArmDocument struct {
	Key    string
	Chunks []EvalChunk
}

func commandNames() []string {
	return []string{"generate", "embed", "pool", "score", "replay", "apply-replay"}
}

func buildArmDocuments(arm string, sources []SourceDocument) []ArmDocument {
	var documents []ArmDocument
	switch arm {
	case ArmOldProduction, ArmOldContext4Singleton:
		for _, source := range sources {
			for i, text := range source.OldChunks {
				// Legacy chunk text is the embedded source itself, so its span
				// covers the whole text (subject-body basis, zero start).
				documents = append(documents, ArmDocument{Key: source.ID + ":old:" + strconv.Itoa(i), Chunks: []EvalChunk{{
					ExternalID: source.ID, SourceID: source.ID, MessageID: source.MessageID, ChunkIndex: i, Text: text,
					SourceEnd: len([]rune(text)),
				}}})
			}
		}
	case ArmStructuredSingleton:
		for _, source := range sources {
			for i, text := range source.StructuredChunks {
				externalID := source.ID
				if i < len(source.StructuredChunkIDs) {
					externalID = source.StructuredChunkIDs[i]
				}
				documents = append(documents, ArmDocument{Key: source.ID + ":structured:" + strconv.Itoa(i), Chunks: []EvalChunk{{
					ExternalID: externalID, SourceID: source.ID, MessageID: source.MessageID, ChunkIndex: i, Text: text,
					SourceStart: indexedInt(source.StructuredChunkStarts, i), SourceEnd: indexedInt(source.StructuredChunkEnds, i),
					SourceBasis: indexedBasis(source.StructuredChunkBases, i),
				}}})
			}
		}
	case ArmNestedContext4:
		byDocument := make(map[string][]EvalChunk)
		for _, source := range sources {
			for i, text := range source.StructuredChunks {
				externalID := source.ID
				if i < len(source.StructuredChunkIDs) {
					externalID = source.StructuredChunkIDs[i]
				}
				byDocument[source.DocumentID] = append(byDocument[source.DocumentID], EvalChunk{
					ExternalID: externalID, SourceID: source.ID, MessageID: source.MessageID, ChunkIndex: i, Text: text,
					SourceStart: indexedInt(source.StructuredChunkStarts, i), SourceEnd: indexedInt(source.StructuredChunkEnds, i),
					SourceBasis: indexedBasis(source.StructuredChunkBases, i),
				})
			}
		}
		keys := make([]string, 0, len(byDocument))
		for key := range byDocument {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			documents = append(documents, ArmDocument{Key: key, Chunks: byDocument[key]})
		}
	default:
		panic("unknown evaluation arm: " + arm)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].Key < documents[j].Key })
	return documents
}

func indexedInt(values []int, index int) int {
	if index < len(values) {
		return values[index]
	}
	return 0
}

func indexedBasis(values []vector.SourceBasis, index int) vector.SourceBasis {
	if index < len(values) {
		return values[index]
	}
	return vector.SourceBasisSubjectBody
}

func flattenChunkText(documents []ArmDocument) []string {
	var texts []string
	for _, document := range documents {
		for _, chunk := range document.Chunks {
			texts = append(texts, chunk.Text)
		}
	}
	return texts
}

type queryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

type queryCache struct {
	client queryEmbedder
	mu     sync.Mutex
	values map[string][]float32
}

func newQueryCache(client queryEmbedder) *queryCache {
	return &queryCache{client: client, values: make(map[string][]float32)}
}

func (c *queryCache) ForArm(ctx context.Context, arm, query string) ([]float32, error) {
	if arm == ArmOldProduction {
		return c.client.EmbedQuery(ctx, query)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if vector, ok := c.values[query]; ok {
		return append([]float32(nil), vector...), nil
	}
	value, err := c.client.EmbedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	c.values[query] = append([]float32(nil), value...)
	return value, nil
}

type sourceSeed struct {
	externalID string
	family     string
	subject    string
	body       string
}

func assembleScenarioDocuments(ctx context.Context, dir string, scenario Scenario) ([]SourceDocument, func(), error) {
	corpus := Corpus{Scenarios: []Scenario{scenario}, Judgments: map[string]Judgment{
		scenario.ID: {ScenarioID: scenario.ID, Grades: map[string]int{scenario.PositiveID: 3, scenario.HardNegativeID: 0}},
	}}
	documents, _, _, cleanup, err := assembleStructuredCorpus(ctx, dir, corpus)
	return documents, cleanup, err
}

func assembleStructuredCorpus(ctx context.Context, dir string, corpus Corpus) ([]SourceDocument, *store.Store, string, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", func() {}, err
	}
	path := filepath.Join(dir, "source.db")
	st, err := store.OpenForTest(path)
	if err != nil {
		return nil, nil, "", func() {}, fmt.Errorf("open evaluator source store: %w", err)
	}
	cleanup := func() {
		_ = st.Close()
		_ = os.RemoveAll(dir)
	}
	if err := st.InitSchema(); err != nil {
		cleanup()
		return nil, nil, "", func() {}, fmt.Errorf("initialize evaluator source store: %w", err)
	}

	seeds, scopes, err := seedScenarioRows(ctx, st, corpus.Scenarios, corpus.Distractors)
	if err != nil {
		cleanup()
		return nil, nil, "", func() {}, err
	}
	snapshot, err := embed.BeginSourceSnapshot(ctx, st)
	if err != nil {
		cleanup()
		return nil, nil, "", func() {}, err
	}
	policy := evaluationAssemblyPolicy()
	assembler := embed.CompositeAssembler{Policy: policy, Chat: embed.ChatWindowAssembler{Policy: policy}}
	documents, err := assembler.AssembleScopes(ctx, snapshot, scopes)
	closeErr := snapshot.Close()
	if err != nil {
		cleanup()
		return nil, nil, "", func() {}, fmt.Errorf("assemble evaluator documents: %w", err)
	}
	if closeErr != nil {
		cleanup()
		return nil, nil, "", func() {}, closeErr
	}

	sources, err := sourceDocumentsFromAssembly(documents, seeds)
	if err != nil {
		cleanup()
		return nil, nil, "", func() {}, err
	}
	return append(sources, corpus.Distractors...), st, path, cleanup, nil
}

func evaluationAssemblyPolicy() embed.AssemblyPolicy {
	return embed.AssemblyPolicy{
		MaxChunkRunes: evaluationChunkRunes, MaxDocumentUTF8Bytes: evaluationDocumentMax,
		ChatGap: 30 * time.Minute, Preprocess: productionPreprocessConfig(),
	}
}

func evaluationPolicyFingerprint() string {
	policy := evaluationAssemblyPolicy()
	oldConfig := evaluationVectorConfig(ArmOldProduction)
	contextConfig := evaluationVectorConfig(ArmNestedContext4)
	chunkPolicy := embed.EmbeddingChunkPolicy(evaluationChunkRunes)
	return fmt.Sprintf("assembly:max_chunk=%d,max_document_bytes=%d,chat_gap=%s,overlap=%d,max_spans=%d;old=%s;context=%s",
		policy.MaxChunkRunes, policy.MaxDocumentUTF8Bytes, policy.ChatGap, chunkPolicy.OverlapRunes, chunkPolicy.MaxSpans,
		oldConfig.GenerationFingerprint(), contextConfig.GenerationFingerprint())
}

func evaluationVectorConfig(arm string) vector.Config {
	model := context4Model
	format := vector.APIFormatVoyageContextual
	if arm == ArmOldProduction {
		model = oldProductionModel
		format = vector.APIFormatOpenAI
	}
	enabled := true
	config := vector.Config{Backend: "sqlite-vec", Preprocess: vector.PreprocessConfig{
		StripQuotes: &enabled, StripSignatures: &enabled, StripHTML: &enabled,
		StripBase64: &enabled, StripURLTracking: &enabled, CollapseWhitespace: &enabled,
	}}
	config.Embeddings.APIFormat = format
	config.Embeddings.Endpoint = "https://api.voyageai.com/v1"
	config.Embeddings.Model = model
	config.Embeddings.Dimension = evaluationDimension
	config.ApplyDefaults()
	return config
}

func armConfigReport(arm string, config vector.Config) ArmConfigReport {
	boundary := map[string]string{
		ArmOldProduction:        "production_worker",
		ArmOldContext4Singleton: "production_preprocess_chunk_adapter",
		ArmStructuredSingleton:  "production_assembler_stream_adapter",
		ArmNestedContext4:       "production_context_worker",
	}[arm]
	return ArmConfigReport{GenerationFingerprint: config.GenerationFingerprint(),
		MaxInputChars: config.Embeddings.MaxInputChars, BatchSize: config.Embeddings.BatchSize,
		APIFormat: string(config.Embeddings.EffectiveAPIFormat()), BuilderBoundary: boundary}
}

func seedScenarioRows(ctx context.Context, st *store.Store, scenarios []Scenario, distractors []SourceDocument) (map[int64]sourceSeed, []embed.AffectedScope, error) {
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sources (id, source_type, identifier, display_name)
		VALUES (1, 'contextual_eval', 'synthetic-evaluator', 'Synthetic Evaluator')`); err != nil {
		return nil, nil, fmt.Errorf("seed evaluator source: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO participants (id, display_name) VALUES
		(1, 'Agent Alpha'), (2, 'Agent Beta')`); err != nil {
		return nil, nil, fmt.Errorf("seed evaluator participants: %w", err)
	}

	seeds := make(map[int64]sourceSeed)
	ftsSeeds := make(map[int64]sourceSeed)
	var scopes []embed.AffectedScope
	messageID, conversationID := int64(1), int64(1)
	for _, scenario := range scenarios {
		for _, relevant := range []bool{true, false} {
			externalID := scenario.HardNegativeID
			if relevant {
				externalID = scenario.PositiveID
			}
			contextText, answerText := scenarioTexts(scenario, relevant)
			title := fmt.Sprintf("Synthetic %s record", scenario.Family)
			conversationType := "email_thread"
			if scenario.Family == familyChat {
				conversationType = "group_chat"
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversations
				(id, source_id, source_conversation_id, conversation_type, title)
				VALUES (?, 1, ?, ?, ?)`, conversationID, fmt.Sprintf("eval-conversation-%d", conversationID), conversationType, title); err != nil {
				return nil, nil, fmt.Errorf("seed evaluator conversation: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_participants
				(conversation_id, participant_id) VALUES (?, 1), (?, 2)`, conversationID, conversationID); err != nil {
				return nil, nil, fmt.Errorf("seed evaluator conversation participants: %w", err)
			}

			switch scenario.Family {
			case familyChat:
				contextID := messageID
				if err := insertEvaluatorMessage(ctx, tx, contextID, conversationID, "beeper", "", contextText, 1, "2030-01-01 09:00:00"); err != nil {
					return nil, nil, err
				}
				seeds[contextID] = sourceSeed{externalID: externalID + "-evidence", family: scenario.Family, body: contextText}
				ftsSeeds[contextID] = seeds[contextID]
				messageID++
				answerID := messageID
				if err := insertEvaluatorMessage(ctx, tx, answerID, conversationID, "beeper", "", answerText, 2, "2030-01-01 09:01:00"); err != nil {
					return nil, nil, err
				}
				seeds[answerID] = sourceSeed{externalID: externalID, family: scenario.Family, body: answerText}
				ftsSeeds[answerID] = seeds[answerID]
				scopes = append(scopes, embed.ChatMessageScope(conversationID,
					time.Date(2030, 1, 1, 9, 1, 0, 0, time.UTC), answerID))
				messageID++
			case familyTranscript:
				// Each turn is below the production 32,768-rune limit, but their
				// combination is above it. The production meeting assembler must
				// therefore keep evidence and answer in distinct owned chunks.
				filler := strings.Repeat(" synthetic-padding", 1700)
				body := title + "\nWhen: 2030-01-01 09:00 UTC\nAttendees: Speaker One, Speaker Two\n\nTranscript:\n" +
					"[00:00] Speaker One: " + contextText + filler + "\n" +
					"[00:01] Speaker Two: " + answerText + filler
				if err := insertEvaluatorMessage(ctx, tx, messageID, conversationID, "meeting_transcript", "", body, 1, "2030-01-01 09:00:00"); err != nil {
					return nil, nil, err
				}
				seeds[messageID] = sourceSeed{externalID: externalID, family: scenario.Family, body: body}
				ftsSeeds[messageID] = seeds[messageID]
				scopes = append(scopes, embed.AffectedScope{Kind: "meeting_transcript", ConversationID: conversationID, MessageID: messageID})
				messageID++
			case familyEmail:
				if err := insertEvaluatorMessage(ctx, tx, messageID, conversationID, familyEmail, title, answerText, 1, "2030-01-01 09:00:00"); err != nil {
					return nil, nil, err
				}
				seeds[messageID] = sourceSeed{externalID: externalID, family: scenario.Family, subject: title, body: answerText}
				ftsSeeds[messageID] = seeds[messageID]
				scopes = append(scopes, embed.AffectedScope{Kind: familyEmail, ConversationID: conversationID, MessageID: messageID})
				messageID++
			default:
				return nil, nil, fmt.Errorf("unknown scenario family %q", scenario.Family)
			}
			conversationID++
		}
	}
	if len(distractors) != 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations
			(id, source_id, source_conversation_id, conversation_type, title)
			VALUES (?, 1, ?, 'email_thread', 'Synthetic distractor archive')`, conversationID,
			fmt.Sprintf("eval-conversation-%d", conversationID)); err != nil {
			return nil, nil, fmt.Errorf("seed distractor conversation: %w", err)
		}
		for _, distractor := range distractors {
			if err := insertEvaluatorMessage(ctx, tx, distractor.MessageID, conversationID, familyEmail,
				distractor.Subject, distractor.Body, 1, "2029-01-01 00:00:00"); err != nil {
				return nil, nil, err
			}
			ftsSeeds[distractor.MessageID] = sourceSeed{externalID: distractor.ID, family: distractor.Family,
				subject: distractor.Subject, body: distractor.Body}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit evaluator source: %w", err)
	}
	messageIDs := make([]int64, 0, len(ftsSeeds))
	for id := range ftsSeeds {
		messageIDs = append(messageIDs, id)
	}
	slices.Sort(messageIDs)
	for _, id := range messageIDs {
		seed := ftsSeeds[id]
		if err := st.UpsertFTS(id, seed.subject, seed.body, "", "", ""); err != nil {
			return nil, nil, fmt.Errorf("seed evaluator FTS for message %d: %w", id, err)
		}
	}
	return seeds, scopes, nil
}

func scenarioTexts(scenario Scenario, relevant bool) (string, string) {
	caseNumber := strings.TrimPrefix(scenario.ID, scenario.Family+"-")
	contextText := fmt.Sprintf("Context for %s case %s: %s.", scenario.Family, caseNumber, scenario.Query)
	if scenario.ContextOnly && !relevant {
		contextText = fmt.Sprintf("Context for %s case %s: matched unrelated synthetic comparison.", scenario.Family, caseNumber)
	}
	answer := fmt.Sprintf("Confirmed action %s. Proceed with the recorded step.", caseNumber)
	if !scenario.ContextOnly {
		if relevant {
			answer = fmt.Sprintf("The record states %s and approves action %s.", scenario.Query, caseNumber)
		} else {
			answer = fmt.Sprintf("The record mentions %s but keeps action %s under review.", scenario.Query, caseNumber)
		}
	}
	return contextText, answer
}

func insertEvaluatorMessage(ctx context.Context, tx *sql.Tx, id, conversationID int64, messageType, subject, body string, senderID int64, sentAt string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type, sent_at, sender_id, subject)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?)`, id, conversationID, fmt.Sprintf("eval-message-%d", id), messageType, sentAt, senderID, subject); err != nil {
		return fmt.Errorf("seed evaluator message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, id, body); err != nil {
		return fmt.Errorf("seed evaluator body: %w", err)
	}
	return nil
}

func sourceDocumentsFromAssembly(documents []embed.Document, seeds map[int64]sourceSeed) ([]SourceDocument, error) {
	byMessage := make(map[int64]*SourceDocument)
	for _, document := range documents {
		for _, chunk := range document.Chunks {
			seed, ok := seeds[chunk.MessageID]
			if !ok {
				return nil, fmt.Errorf("assembled unknown message %d", chunk.MessageID)
			}
			source := byMessage[chunk.MessageID]
			if source == nil {
				oldChunks, err := productionOldChunks(seed.subject, seed.body)
				if err != nil {
					return nil, fmt.Errorf("build current production chunks for message %d: %w", chunk.MessageID, err)
				}
				source = &SourceDocument{
					ID: seed.externalID, MessageID: chunk.MessageID, Family: seed.family,
					DocumentID: document.Key, Subject: seed.subject, Body: seed.body, OldChunks: oldChunks,
				}
				byMessage[chunk.MessageID] = source
			}
			source.StructuredChunks = append(source.StructuredChunks, chunk.Text)
			chunkID := source.ID
			if source.Family == familyTranscript && len(document.Chunks) > 1 {
				chunkID = source.ID + "-context"
				if len(source.StructuredChunks) == 2 {
					chunkID = source.ID + "-evidence"
				}
			}
			source.StructuredChunkIDs = append(source.StructuredChunkIDs, chunkID)
			source.StructuredChunkStarts = append(source.StructuredChunkStarts, chunk.SourceCharStart)
			source.StructuredChunkEnds = append(source.StructuredChunkEnds, chunk.SourceCharEnd)
			source.StructuredChunkBases = append(source.StructuredChunkBases, chunk.SourceBasis)
		}
	}
	result := make([]SourceDocument, 0, len(byMessage))
	for _, source := range byMessage {
		result = append(result, *source)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].DocumentID == result[j].DocumentID {
			return result[i].MessageID < result[j].MessageID
		}
		return result[i].DocumentID < result[j].DocumentID
	})
	return result, nil
}

func productionOldChunks(subject, body string) ([]string, error) {
	text, _ := embed.Preprocess(subject, body, 0, productionPreprocessConfig())
	spans, tailDropped := embed.EmbeddingChunkPolicy(evaluationChunkRunes).Chunk(text)
	if tailDropped {
		return nil, errors.New("current production chunk span cap truncated evaluator source")
	}
	chunks := make([]string, 0, len(spans))
	for _, span := range spans {
		chunks = append(chunks, span.Text)
	}
	return chunks, nil
}

type documentEmbedder interface {
	EmbedDocuments(ctx context.Context, documents []embed.DocumentInput) ([][][]float32, error)
}

type armIndex struct {
	backend              *sqlitevec.Backend
	generation           vector.GenerationID
	path                 string
	idByNumber           map[int64]string
	numberByID           map[string]int64
	chunkIDByKey         map[string]string
	documentIDByKey      map[string]string
	hybridImplementation string
}

func buildFreshArmIndex(ctx context.Context, dir, arm string, documents []ArmDocument, client documentEmbedder, mainStore *store.Store, mainPath string) (*armIndex, ArmReport, error) {
	start := time.Now()
	path := filepath.Join(dir, arm+".db")
	if _, err := os.Stat(path); err == nil {
		return nil, ArmReport{}, fmt.Errorf("refusing non-fresh %s vector backend %s", arm, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ArmReport{}, err
	}
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{Path: path, MainDB: mainStore.DB(), MainPath: mainPath, Dimension: evaluationDimension})
	if err != nil {
		return nil, ArmReport{}, fmt.Errorf("open %s vector backend: %w", arm, err)
	}
	fail := func(err error) (*armIndex, ArmReport, error) {
		_ = backend.Close()
		return nil, ArmReport{}, err
	}
	model := context4Model
	if arm == ArmOldProduction {
		model = oldProductionModel
	}
	config := evaluationVectorConfig(arm)
	generation, err := backend.CreateGeneration(ctx, model, evaluationDimension, config.GenerationFingerprint())
	if err != nil {
		return fail(err)
	}

	inputs := make([]embed.DocumentInput, len(documents))
	for i, document := range documents {
		inputs[i].Chunks = make([]string, len(document.Chunks))
		for j, chunk := range document.Chunks {
			inputs[i].Chunks[j] = chunk.Text
		}
	}
	var latencies []time.Duration
	workerStart := time.Now()
	switch arm {
	case ArmOldProduction:
		legacy, ok := client.(embed.EmbeddingClient)
		if !ok {
			return fail(errors.New("o-prod requires the production embedding client boundary"))
		}
		worker := embed.NewWorker(embed.WorkerDeps{Backend: backend, VectorsDB: backend.DB(), MainDB: mainStore.DB(),
			Store: mainStore, Client: legacy, Preprocess: productionPreprocessConfig(),
			MaxInputChars: config.Embeddings.MaxInputChars, BatchSize: config.Embeddings.BatchSize, Recorder: mainStore})
		if _, err := worker.RunOnce(ctx, generation, evaluationPassScope(
			fmt.Sprintf("contextual-eval:index:%s:pass:1", arm),
		)); err != nil {
			return fail(fmt.Errorf("run production O-prod worker: %w", err))
		}
		latencies = append(latencies, time.Since(workerStart))
	case ArmNestedContext4:
		semantic, ok := client.(embed.SemanticClient)
		if !ok {
			return fail(errors.New("N-c4 requires the production SemanticClient boundary"))
		}
		policy := evaluationAssemblyPolicy()
		worker := embed.NewContextWorker(embed.ContextWorkerDeps{Backend: backend, Publisher: backend, Store: mainStore,
			Assembler: embed.CompositeAssembler{Policy: policy, Chat: embed.ChatWindowAssembler{Policy: policy}},
			Client:    semantic, ChangeBatchSize: 64, ReconcileBatchSize: 128, Recorder: mainStore})
		if _, err := runContextWorkerUntilConverged(ctx, worker, generation,
			"contextual-eval:index:"+arm); err != nil {
			return fail(fmt.Errorf("run production N-c4 context worker: %w", err))
		}
		coverage, err := mainStore.ContextualConvergenceCounts(ctx, int64(generation))
		if err != nil {
			return fail(fmt.Errorf("read production N-c4 coverage: %w", err))
		}
		if coverage.Missing != 0 || coverage.Stamped != coverage.Live {
			return fail(fmt.Errorf("production N-c4 coverage incomplete: stamped=%d live=%d missing=%d",
				coverage.Stamped, coverage.Live, coverage.Missing))
		}
		latencies = append(latencies, time.Since(workerStart))
	default:
		latencies, err = streamEmbedArmDocuments(ctx, generation, backend, documents,
			config.Embeddings.BatchSize, client)
		if err != nil {
			return fail(fmt.Errorf("embed %s documents: %w", arm, err))
		}
	}

	idByNumber := make(map[int64]string)
	numberByID := make(map[string]int64)
	chunkIDByKey := make(map[string]string)
	documentIDByKey := make(map[string]string)
	for _, document := range documents {
		for _, owner := range document.Chunks {
			idByNumber[owner.MessageID] = owner.SourceID
			numberByID[owner.SourceID] = owner.MessageID
			key := fmt.Sprintf("%d:%d", owner.MessageID, owner.ChunkIndex)
			chunkIDByKey[key] = owner.ExternalID
			documentIDByKey[key] = document.Key
		}
	}
	if _, err := backend.DB().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fail(fmt.Errorf("checkpoint %s vector backend: %w", arm, err))
	}
	indexBytes, err := indexFilesBytes(path)
	if err != nil {
		return fail(err)
	}
	// #nosec G101 -- these are evaluator accounting labels, not credentials.
	report := ArmReport{
		Model: model, Requests: int64(len(latencies)), DocumentRequests: int64(len(latencies)),
		RequestAccounting: "client_batches_excluding_internal_retries",
		InputTokens:       estimatedTokens(inputs), TokenAccounting: "utf8_bytes_divided_by_four_estimate",
		LatencyMillis: summarizeLatencies(latencies),
		Build: ResourceSummary{WallMillis: float64(time.Since(start).Milliseconds()),
			MemoryMeasurement: "awaiting_parent_wait4", IndexBytes: indexBytes},
		Config: armConfigReport(arm, config),
	}
	return &armIndex{backend: backend, generation: generation, path: path, idByNumber: idByNumber,
		numberByID: numberByID, chunkIDByKey: chunkIDByKey, documentIDByKey: documentIDByKey,
		hybridImplementation: "sqlitevec.FusedSearch"}, report, nil
}

type contextWorkerRunner interface {
	RunOnce(ctx context.Context, generation vector.GenerationID, scope operations.PassScope) (embed.RunResult, error)
}

func runContextWorkerUntilConverged(
	ctx context.Context, worker contextWorkerRunner, generation vector.GenerationID, scopePrefix string,
) (embed.RunResult, error) {
	var total embed.RunResult
	for passIndex := 0; ; passIndex++ {
		pass, err := worker.RunOnce(ctx, generation, evaluationPassScope(
			fmt.Sprintf("%s:pass:%d", scopePrefix, passIndex+1),
		))
		if err != nil {
			return total, err
		}
		total.Claimed += pass.Claimed
		total.Succeeded += pass.Succeeded
		total.Failed += pass.Failed
		total.Truncated += pass.Truncated
		total.Contextual = pass.Contextual
		if pass.Contextual != nil && pass.Contextual.Converged {
			return total, nil
		}
		if pass.Claimed == 0 && pass.Succeeded == 0 && pass.Failed == 0 && pass.Truncated == 0 {
			return total, errors.New("production N-c4 worker made no progress before convergence")
		}
	}
}

func evaluationPassScope(key string) operations.PassScope {
	return operations.PassScope{
		Key: key, Trigger: operations.TriggerManual, StartedAt: time.Now().UTC(),
	}
}

func indexFilesBytes(path string) (int64, error) {
	var total int64
	for _, candidate := range []string{path, path + "-wal"} {
		stat, err := os.Stat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += stat.Size()
	}
	if total == 0 {
		return 0, fmt.Errorf("vector index files are missing for %s", path)
	}
	return total, nil
}

func streamEmbedArmDocuments(ctx context.Context, generation vector.GenerationID, backend *sqlitevec.Backend,
	documents []ArmDocument, batchSize int, client documentEmbedder) ([]time.Duration, error) {
	if batchSize <= 0 {
		return nil, errors.New("document batch size must be positive")
	}
	latencies := make([]time.Duration, 0, (len(documents)+batchSize-1)/batchSize)
	for start := 0; start < len(documents); {
		end := documentBatchEnd(documents, start, batchSize)
		inputs := make([]embed.DocumentInput, end-start)
		for i, document := range documents[start:end] {
			inputs[i].Chunks = make([]string, len(document.Chunks))
			for j, chunk := range document.Chunks {
				inputs[i].Chunks[j] = chunk.Text
			}
		}
		requestStart := time.Now()
		vectors, err := client.EmbedDocuments(ctx, inputs)
		latencies = append(latencies, time.Since(requestStart))
		if err != nil {
			return latencies, err
		}
		if len(vectors) != end-start {
			return latencies, fmt.Errorf("batch response count mismatch: got %d want %d", len(vectors), end-start)
		}
		chunks := make([]vector.Chunk, 0)
		for i, document := range documents[start:end] {
			if len(vectors[i]) != len(document.Chunks) {
				return latencies, fmt.Errorf("vector chunk count mismatch for %s", document.Key)
			}
			chunks = append(chunks, documentVectorChunks(document, vectors[i])...)
		}
		if err := backend.Upsert(ctx, generation, chunks); err != nil {
			return latencies, err
		}
		start = end
	}
	return latencies, nil
}

// documentVectorChunks mirrors production span semantics: ChunkCharStart/End
// are the chunk's raw source span — contextual headers in the embedded text
// must never inflate them into false evidence overlaps — and SourceCharLen is
// the message's maximum source span end within the document.
func documentVectorChunks(document ArmDocument, vectors [][]float32) []vector.Chunk {
	sourceLengths := make(map[int64]int)
	for _, owner := range document.Chunks {
		sourceLengths[owner.MessageID] = max(sourceLengths[owner.MessageID], owner.SourceEnd)
	}
	chunks := make([]vector.Chunk, 0, len(document.Chunks))
	for j, owner := range document.Chunks {
		chunks = append(chunks, vector.Chunk{MessageID: owner.MessageID, ChunkIndex: owner.ChunkIndex,
			Vector: vectors[j], SourceCharLen: sourceLengths[owner.MessageID],
			ChunkCharStart: owner.SourceStart, ChunkCharEnd: owner.SourceEnd, SourceBasis: owner.SourceBasis})
	}
	return chunks
}

func documentBatchEnd(documents []ArmDocument, start, batchSize int) int {
	end := min(start+batchSize, len(documents))
	lastByMessage := make(map[int64]int)
	for i := start; i < len(documents); i++ {
		for _, chunk := range documents[i].Chunks {
			lastByMessage[chunk.MessageID] = i
		}
	}
	for i := start; i < end; i++ {
		for _, chunk := range documents[i].Chunks {
			end = max(end, lastByMessage[chunk.MessageID]+1)
		}
	}
	return end
}

func runArmEvaluationIsolated(ctx context.Context, dir, arm string, input armEvalInput, endpoint, apiKey string,
	mainStore *store.Store, mainPath string, fallbackClient documentEmbedder) (armEvalOutput, error) {
	executable, err := os.Executable()
	if err != nil || strings.HasSuffix(executable, ".test") {
		index, report, buildErr := buildFreshArmIndex(ctx, dir, arm, input.Documents, fallbackClient, mainStore, mainPath)
		if buildErr != nil {
			return armEvalOutput{}, buildErr
		}
		defer func() { _ = index.Close() }()
		output, evalErr := evaluateArmIndex(ctx, index, report, input)
		output.Report.Build.PeakRSSBytes = nil
		output.Report.Build.MemoryMeasurement = "unavailable_test_process_not_isolated"
		return output, evalErr
	}
	inputPath := filepath.Join(dir, arm+"-evaluation.json")
	if err := writeJSON(inputPath, input); err != nil {
		return armEvalOutput{}, err
	}
	// #nosec G204 -- executable is os.Executable and the child subcommand arguments are fixed evaluator inputs.
	cmd := exec.CommandContext(ctx, executable, "arm-run", "--arm", arm, "--input", inputPath,
		"--main", mainPath, "--dir", dir, "--endpoint", endpoint)
	cmd.Env = append(os.Environ(), "MSGVAULT_EVAL_CHILD_API_KEY="+apiKey)
	measured := runMeasuredChildCommand(cmd)
	if measured.ExitError != "" {
		return armEvalOutput{}, fmt.Errorf("isolated %s evaluation failed: %s", arm, measured.ExitError)
	}
	var output armEvalOutput
	if err := json.Unmarshal(measured.Output, &output); err != nil {
		return armEvalOutput{}, fmt.Errorf("decode isolated %s evaluation: %w", arm, err)
	}
	output.Report.Build.PeakRSSBytes = measured.MaxRSSBytes
	output.Report.Build.MemoryMeasurement = measured.MemoryMeasurement
	return output, nil
}

func evaluateArmIndex(ctx context.Context, index *armIndex, report ArmReport, input armEvalInput) (armEvalOutput, error) {
	output := armEvalOutput{Report: report, Rankings: make(map[string][]string), Results: make(map[string]rankingResult),
		Provenance: make(map[string]map[string][]chunkEvidence)}
	for _, item := range input.Scenarios {
		annStart := time.Now()
		ann, err := index.Search(ctx, item.Query, 20)
		if err != nil {
			return armEvalOutput{}, err
		}
		annTime := time.Since(annStart)
		l2, err := index.ExactTopK(ctx, item.Query, "l2", 20)
		if err != nil {
			return armEvalOutput{}, err
		}
		var cosine []string
		if input.Exact {
			cosine, err = index.ExactTopK(ctx, item.Query, "cosine", 20)
			if err != nil {
				return armEvalOutput{}, err
			}
		}
		hybrid, _, err := index.FusedSearch(ctx, item.Query, strings.Fields(item.Scenario.Query), 20)
		if err != nil {
			return armEvalOutput{}, err
		}
		metrics := scoreRanking(ann, l2, cosine, item.Judgment)
		evidence, err := index.EvidenceRanking(ctx, ann, item.Query)
		if err != nil {
			return armEvalOutput{}, err
		}
		metrics.EvidenceHitAt10 = evidenceHitAt(evidence, item.Judgment.EvidenceIDs, 10)
		annRefs, err := index.EvidenceRefs(ctx, ann, item.Query)
		if err != nil {
			return armEvalOutput{}, err
		}
		if len(item.Judgment.EvidenceRefs) > 0 {
			metrics.EvidenceHitAt10 = evidenceSpanHitAt(annRefs, item.Judgment.EvidenceRefs, 10)
		}
		hybridRefs, err := index.EvidenceRefs(ctx, hybrid, item.Query)
		if err != nil {
			return armEvalOutput{}, err
		}
		output.Provenance[item.Scenario.ID] = map[string][]chunkEvidence{"ann": annRefs, "hybrid": hybridRefs}
		output.Rankings[item.Scenario.ID] = ann
		output.Results[item.Scenario.ID] = rankingResult{ANN: ann, Exact: l2, L2: cosine, Hybrid: hybrid,
			Metrics: metrics, HybridNDCG: ndcgAt10(gradesForRanking(hybrid, item.Judgment), gradeValues(item.Judgment.Grades)),
			ANNTime: annTime, FullTime: item.QueryTime + annTime}
	}
	norms, err := index.NormSummary(ctx)
	if err != nil {
		return armEvalOutput{}, err
	}
	output.Report.VectorNorms = norms
	return output, nil
}

func embedDocumentBatches(ctx context.Context, inputs []embed.DocumentInput, batchSize int, client documentEmbedder) ([][][]float32, []time.Duration, error) {
	if batchSize <= 0 {
		return nil, nil, errors.New("document batch size must be positive")
	}
	vectors := make([][][]float32, 0, len(inputs))
	latencies := make([]time.Duration, 0, (len(inputs)+batchSize-1)/batchSize)
	for startIndex := 0; startIndex < len(inputs); startIndex += batchSize {
		endIndex := min(startIndex+batchSize, len(inputs))
		embedStart := time.Now()
		batch, err := client.EmbedDocuments(ctx, inputs[startIndex:endIndex])
		latencies = append(latencies, time.Since(embedStart))
		if err != nil {
			return nil, latencies, err
		}
		if len(batch) != endIndex-startIndex {
			return nil, latencies, fmt.Errorf("batch response count mismatch: got %d want %d", len(batch), endIndex-startIndex)
		}
		vectors = append(vectors, batch...)
	}
	return vectors, latencies, nil
}

func (i *armIndex) Close() error { return i.backend.Close() }

func (i *armIndex) Search(ctx context.Context, query []float32, k int) ([]string, error) {
	hits, err := i.backend.Search(ctx, i.generation, query, k, vector.Filter{})
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(hits))
	for _, hit := range hits {
		id, ok := i.idByNumber[hit.MessageID]
		if !ok {
			return nil, fmt.Errorf("ANN returned unknown message %d", hit.MessageID)
		}
		result = append(result, id)
	}
	return result, nil
}

func (i *armIndex) FusedSearch(ctx context.Context, query []float32, terms []string, k int) ([]string, bool, error) {
	hits, saturated, err := i.backend.FusedSearch(ctx, vector.FusedRequest{
		FTSTerms: terms, QueryVec: query, Generation: i.generation,
		KPerSignal: 100, Limit: k, RRFK: 60, SubjectBoost: 2,
	})
	if err != nil {
		return nil, false, err
	}
	result := make([]string, 0, len(hits))
	for _, hit := range hits {
		id, ok := i.idByNumber[hit.MessageID]
		if !ok {
			return nil, false, fmt.Errorf("hybrid returned unknown message %d", hit.MessageID)
		}
		result = append(result, id)
	}
	return result, saturated, nil
}

func (i *armIndex) ExactTopK(ctx context.Context, query []float32, distance string, k int) ([]string, error) {
	if k <= 0 {
		return nil, nil
	}
	function := "vec_distance_L2"
	if distance == "cosine" {
		function = "vec_distance_cosine"
	} else if distance != "l2" {
		return nil, fmt.Errorf("unknown exact distance %q", distance)
	}
	querySQL := fmt.Sprintf(`SELECT e.message_id, MIN(%s(v.embedding, ?)) AS distance
		FROM %s v JOIN embeddings e ON e.embedding_id = v.embedding_id
		WHERE v.generation_id = ? GROUP BY e.message_id
		ORDER BY distance ASC, e.message_id ASC LIMIT ?`, function, sqlitevec.VectorTableName(evaluationDimension))
	rows, err := i.backend.DB().QueryContext(ctx, querySQL, evaluatorVectorBlob(query), int64(i.generation), k)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0, k)
	for rows.Next() {
		var messageID int64
		var value float64
		if err := rows.Scan(&messageID, &value); err != nil {
			return nil, err
		}
		if math.IsNaN(value) {
			continue
		}
		id, ok := i.idByNumber[messageID]
		if !ok {
			return nil, fmt.Errorf("exact oracle returned unknown message %d", messageID)
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func evaluatorVectorBlob(vector []float32) []byte {
	buffer := make([]byte, len(vector)*4)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(buffer[i*4:], math.Float32bits(value))
	}
	return buffer
}

func evaluatorVectorFromBlob(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("invalid float32 vector blob size %d", len(blob))
	}
	result := make([]float32, len(blob)/4)
	for i := range result {
		result[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return result, nil
}

func (i *armIndex) NormSummary(ctx context.Context) (LatencySummary, error) {
	query := fmt.Sprintf(`SELECT embedding FROM %s WHERE generation_id = ?`, sqlitevec.VectorTableName(evaluationDimension))
	rows, err := i.backend.DB().QueryContext(ctx, query, int64(i.generation))
	if err != nil {
		return LatencySummary{}, err
	}
	defer func() { _ = rows.Close() }()
	var norms []float64
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return LatencySummary{}, err
		}
		value, err := evaluatorVectorFromBlob(blob)
		if err != nil {
			return LatencySummary{}, err
		}
		norms = append(norms, vectorNorm(value))
	}
	if err := rows.Err(); err != nil {
		return LatencySummary{}, err
	}
	sort.Float64s(norms)
	return LatencySummary{P50: percentile(norms, 0.5), P95: percentile(norms, 0.95), P99: percentile(norms, 0.99)}, nil
}

func (i *armIndex) WinningChunkID(ctx context.Context, messageID int64, query []float32) (string, error) {
	winner, err := i.WinningChunkEvidence(ctx, messageID, query)
	return winner.ID, err
}

type chunkEvidence struct {
	ID         string `json:"chunk_id"`
	SourceID   string `json:"source_id"`
	MessageID  int64  `json:"message_id"`
	ChunkIndex int    `json:"chunk_index"`
	DocumentID string `json:"document_id"`
	Start      int    `json:"raw_start"`
	End        int    `json:"raw_end"`
}

func (i *armIndex) WinningChunkEvidence(ctx context.Context, messageID int64, query []float32) (chunkEvidence, error) {
	hits, err := i.backend.ScoreMessageChunks(ctx, i.generation, messageID, query)
	if err != nil {
		return chunkEvidence{}, err
	}
	if len(hits) == 0 {
		return chunkEvidence{}, nil
	}
	key := fmt.Sprintf("%d:%d", messageID, hits[0].ChunkIndex)
	return chunkEvidence{ID: i.chunkIDByKey[key], SourceID: i.idByNumber[messageID], MessageID: messageID,
		ChunkIndex: hits[0].ChunkIndex, DocumentID: i.documentIDByKey[key],
		Start: hits[0].ChunkCharStart, End: hits[0].ChunkCharEnd}, nil
}

func (i *armIndex) EvidenceRanking(ctx context.Context, ranking []string, query []float32) ([]string, error) {
	result := make([]string, 0, len(ranking))
	for _, sourceID := range ranking {
		messageID, ok := i.numberByID[sourceID]
		if !ok {
			continue
		}
		chunkID, err := i.WinningChunkID(ctx, messageID, query)
		if err != nil {
			return nil, err
		}
		if chunkID != "" {
			result = append(result, chunkID)
		}
	}
	return result, nil
}

func (i *armIndex) EvidenceRefs(ctx context.Context, ranking []string, query []float32) ([]chunkEvidence, error) {
	result := make([]chunkEvidence, 0, len(ranking))
	for _, sourceID := range ranking {
		messageID, ok := i.numberByID[sourceID]
		if !ok {
			continue
		}
		winner, err := i.WinningChunkEvidence(ctx, messageID, query)
		if err != nil {
			return nil, err
		}
		if winner.ID != "" {
			result = append(result, winner)
		}
	}
	return result, nil
}

func evidenceSpanHitAt(winners []chunkEvidence, evidence []EvidenceRef, k int) float64 {
	for _, winner := range winners[:min(max(k, 0), len(winners))] {
		for _, ref := range evidence {
			if winner.SourceID == ref.SourceID && winner.Start < ref.RawEnd && ref.RawStart < winner.End {
				return 1
			}
		}
	}
	return 0
}

func estimatedTokens(inputs []embed.DocumentInput) int64 {
	var bytes int64
	for _, input := range inputs {
		for _, chunk := range input.Chunks {
			bytes += int64(len(chunk))
		}
	}
	return (bytes + 3) / 4
}

func validateArm(arm string) error {
	if slices.Contains([]string{ArmOldProduction, ArmOldContext4Singleton, ArmStructuredSingleton, ArmNestedContext4}, arm) {
		return nil
	}
	return errors.New("unknown evaluation arm " + arm)
}
