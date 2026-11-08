package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/eval"
	"go.kenn.io/msgvault/internal/store"
)

type recordingEvalRecord struct {
	ID, Profile, Origin, Text string
	SourceID                  string `json:"source_id"`
	SourceVersionID           string `json:"source_version_id"`
	ContentVersionID          string `json:"content_version_id"`
	SuppliedInputID           string `json:"supplied_input_id"`
	SourceSHA256              string `json:"source_sha256"`
	ByteLength                int64  `json:"byte_length"`
	StartMS                   *int64 `json:"start_ms"`
	EndMS                     *int64 `json:"end_ms"`
}

type recordingEvalManifest struct {
	URL, Profile  string
	APIKey        string         `json:"api_key"`
	VaultUID      string         `json:"vault_uid"`
	Quality       string         `json:"quality"`
	ModelMetadata map[string]any `json:"model_metadata"`
	Records       []recordingEvalRecord
}

type recordingEvalOccurrence struct {
	record                           recordingEvalRecord
	seed                             recordingSeed
	messageKey, conversationKey, ref string
}

type recordingEvalMetric struct {
	Topic                             string                      `json:"topic"`
	Profile                           string                      `json:"profile"`
	RequestedMode                     string                      `json:"requested_mode"`
	ActualMode                        string                      `json:"actual_mode"`
	Coverage                          docbankmedia.SearchCoverage `json:"coverage"`
	Partial                           bool                        `json:"partial"`
	PendingOccurrences                int                         `json:"pending_occurrences"`
	UnavailableOccurrences            int                         `json:"unavailable_occurrences"`
	AttributionUnavailable            int                         `json:"attribution_unavailable"`
	Degradations                      []string                    `json:"degradations,omitempty"`
	Cutoff                            int                         `json:"cutoff"`
	MessagePrecision                  float64                     `json:"message_precision"`
	MessageRecall                     float64                     `json:"message_recall"`
	ConversationPrecision             float64                     `json:"conversation_precision"`
	ConversationRecall                float64                     `json:"conversation_recall"`
	MessageHardNegativesAtCutoff      int                         `json:"message_hard_negatives_at_cutoff"`
	ConversationHardNegativesAtCutoff int                         `json:"conversation_hard_negatives_at_cutoff"`
	Messages                          []string                    `json:"ranked_messages"`
	Conversations                     []string                    `json:"ranked_conversations"`
}

type recordingEvalScenario struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	ForbiddenResults int    `json:"forbidden_results"`
}

type recordingEvalOutput struct {
	Quality       string                  `json:"quality"`
	ModelMetadata map[string]any          `json:"model_metadata,omitempty"`
	CorpusSHA256  string                  `json:"corpus_sha256"`
	Metrics       []recordingEvalMetric   `json:"metrics"`
	Scenarios     []recordingEvalScenario `json:"scenarios"`
}

// TestMediaRetrievalQualification is invoked by the CI runner against an actual
// Docbank API handler with retained synthetic transcripts and vector indexes.
// It never accepts search reports supplied by a mock or reads a live archive.
func TestMediaRetrievalQualification(t *testing.T) {
	manifestPath := os.Getenv("MSGVAULT_MEDIA_EVAL_MANIFEST")
	if manifestPath == "" {
		t.Skip("requires explicit synthetic Docbank qualification manifest")
	}
	raw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var manifest recordingEvalManifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	endpoint, err := url.Parse(manifest.URL)
	require.NoError(t, err)
	require.Equal(t, "http", endpoint.Scheme)
	require.Equal(t, "127.0.0.1", endpoint.Hostname(), "only the isolated synthetic fixture is allowed")
	require.NotEmpty(t, manifest.APIKey)
	require.Len(t, manifest.Records, 4)
	require.Contains(t, []string{"deterministic-contract", "real-model"}, manifest.Quality)
	mode := os.Getenv("MSGVAULT_MEDIA_EVAL_MODE")
	require.Contains(t, []string{"lexical", "semantic", "hybrid"}, mode)
	corpus := filepath.Join("..", "eval", "testdata", "media")
	topics, stats, err := eval.LoadTopics(filepath.Join(corpus, "topics.tsv"))
	require.NoError(t, err)
	require.Zero(t, stats.Skipped)
	require.Len(t, topics, 2)
	messageQrels, stats, err := eval.LoadQrels(filepath.Join(corpus, "qrels_message.txt"))
	require.NoError(t, err)
	require.Zero(t, stats.Skipped)
	conversationQrels, stats, err := eval.LoadQrels(filepath.Join(corpus, "qrels_conversation.txt"))
	require.NoError(t, err)
	require.Zero(t, stats.Skipped)
	output := recordingEvalOutput{Quality: manifest.Quality, ModelMetadata: manifest.ModelMetadata}
	digest := sha256.New()
	for _, name := range []string{"mailbox.json", "topics.tsv", "qrels_message.txt", "qrels_conversation.txt"} {
		contents, err := os.ReadFile(filepath.Join(corpus, name))
		require.NoError(t, err)
		_, _ = fmt.Fprintf(digest, "%s\x00", name)
		_, _ = digest.Write(contents)
	}
	output.CorpusSHA256 = hex.EncodeToString(digest.Sum(nil))
	t.Cleanup(func() {
		data, err := json.MarshalIndent(output, "", "  ")
		require.NoError(t, err)
		if path := os.Getenv("MSGVAULT_MEDIA_EVAL_OUTPUT"); path != "" {
			require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
		}
	})
	observedStatus := 0
	forbiddenResults := 0
	scenario := func(name string, run func(*testing.T)) {
		observedStatus = 0
		forbiddenResults = 0
		status := "passed"
		if !t.Run(name, run) {
			status = "failed"
		}
		output.Scenarios = append(output.Scenarios, recordingEvalScenario{Name: name, Status: status, HTTPStatus: observedStatus, ForbiddenResults: forbiddenResults})
	}
	profiles := []string{manifest.Profile}
	for _, record := range manifest.Records {
		if !slices.Contains(profiles, record.Profile) {
			profiles = append(profiles, record.Profile)
		}
	}

	for _, profile := range profiles {
		scenario("judgments/"+profile, func(t *testing.T) {
			_, server, occurrences := seedRecordingEvaluation(t, manifest, profile)
			byMessage := make(map[int64]recordingEvalOccurrence)
			for _, occurrence := range occurrences {
				byMessage[occurrence.seed.messageID] = occurrence
			}
			for _, topic := range topics {
				require.Len(t, messageQrels[topic.ID], 5, "every corpus occurrence must be judged")
				require.Len(t, conversationQrels[topic.ID], 4, "every corpus conversation must be judged")
				response := requireRecordingEvalSearch(t, server, topic.Query, mode)
				metric := recordingEvalMetric{Topic: topic.ID, Profile: profile, RequestedMode: response.RequestedMode,
					ActualMode: response.ActualMode, Coverage: response.Coverage, Partial: response.Partial, Cutoff: 3,
					PendingOccurrences: response.PendingOccurrences, UnavailableOccurrences: response.UnavailableOccurrences,
					AttributionUnavailable: response.AttributionUnavailable, Degradations: response.Degradations}
				assert.Equal(t, mode, response.RequestedMode)
				assert.Equal(t, mode, response.ActualMode)
				for _, hit := range response.Results {
					occurrence, known := byMessage[hit.MessageID]
					if !known {
						forbiddenResults++
					}
					require.True(t, known, "forbidden result outside the current occurrence map")
					assert.Equal(t, occurrence.seed.attachmentID, hit.AttachmentID)
					assert.Equal(t, occurrence.record.Origin, hit.Origin)
					assert.NotEmpty(t, strings.TrimSpace(hit.Excerpt))
					assert.Equal(t, occurrence.record.StartMS, hit.StartMS)
					assert.Equal(t, occurrence.record.EndMS, hit.EndMS)
					metric.Messages = append(metric.Messages, occurrence.messageKey)
					metric.Conversations = append(metric.Conversations, occurrence.conversationKey)
				}
				metric.Messages = eval.DedupeKeys(metric.Messages)
				metric.Conversations = eval.DedupeKeys(metric.Conversations)
				metric.MessagePrecision = eval.PrecisionAt(metric.Messages, messageQrels.RelevantSet(topic.ID), metric.Cutoff)
				metric.MessageRecall = eval.RecallAt(metric.Messages, messageQrels.RelevantSet(topic.ID), metric.Cutoff)
				metric.ConversationPrecision = eval.PrecisionAt(metric.Conversations, conversationQrels.RelevantSet(topic.ID), metric.Cutoff)
				metric.ConversationRecall = eval.RecallAt(metric.Conversations, conversationQrels.RelevantSet(topic.ID), metric.Cutoff)
				for _, key := range eval.TruncateKeys(metric.Messages, metric.Cutoff) {
					grade, exists := messageQrels[topic.ID][key]
					require.True(t, exists, "missing corpus judgment")
					if grade == 0 {
						metric.MessageHardNegativesAtCutoff++
					}
				}
				for _, key := range eval.TruncateKeys(metric.Conversations, metric.Cutoff) {
					grade, exists := conversationQrels[topic.ID][key]
					require.True(t, exists, "missing conversation judgment")
					if grade == 0 {
						metric.ConversationHardNegativesAtCutoff++
					}
				}
				if topic.ID == "q1" && mode == "lexical" {
					assert.ElementsMatch(t, []string{"<media-a-v1-1@example.test>", "<media-a-v1-2@example.test>"}, metric.Messages)
				}
				if topic.ID == "q2" && mode != "lexical" {
					assert.Positive(t, metric.MessagePrecision, "a semantic query must retrieve a relevant occurrence within the declared cutoff")
					if profile == manifest.Profile {
						assert.Contains(t, metric.Messages, "<media-a-v1-1@example.test>")
						assert.Contains(t, metric.Messages, "<media-a-v1-2@example.test>")
					} else {
						assert.Contains(t, metric.Messages, "<media-b-v1@example.test>")
					}
					assert.True(t, response.Partial, "one profile must not claim complete mixed-profile coverage")
				}
				output.Metrics = append(output.Metrics, metric)
			}
		})
	}

	scenario("empty-local-scope", func(t *testing.T) {
		f := newRecordingFixture(t)
		f.client = recordingEvalClient(t, manifest)
		server := f.server(true)
		server.messageRecordings.SearchProfile = manifest.Profile
		response := requireRecordingEvalSearch(t, server, "telescope", mode)
		assert.Empty(t, response.Results)
		if mode != "lexical" {
			assert.Equal(t, "unavailable", response.ActualMode)
			assert.True(t, response.Partial)
		}
	})
	scenario("no-selected-transcript", func(t *testing.T) {
		f := newRecordingFixture(t)
		f.client = recordingEvalClient(t, manifest)
		record := manifest.Records[0]
		record.SuppliedInputID, record.Text = manifest.Records[1].SuppliedInputID, manifest.Records[1].Text
		seedRecordingEvalOccurrence(t, f, manifest, record, "excluded-caption", "excluded-caption", "excluded-caption")
		server := f.server(true)
		server.messageRecordings.SearchProfile = manifest.Profile
		if mode != "lexical" {
			require.NoError(t, recordingEvalControl(t.Context(), manifest, "arm", ""))
			defer func() {
				require.NoError(t, recordingEvalControl(context.WithoutCancel(t.Context()), manifest, "release", ""))
			}()
		}
		response := requireRecordingEvalSearch(t, server, "telescope", mode)
		assert.Empty(t, response.Results)
		assert.True(t, response.Partial)
		assert.Equal(t, 1, response.UnavailableOccurrences)
		assert.Equal(t, 0, response.Coverage.CompleteDocuments)
		if mode != "lexical" {
			assert.Equal(t, "unavailable", response.ActualMode)
		}
	})
	scenario("selected-build-without-profile-vectors", func(t *testing.T) {
		f := newRecordingFixture(t)
		f.client = recordingEvalClient(t, manifest)
		var generated recordingEvalRecord
		for _, record := range manifest.Records {
			if record.Origin == "generated" {
				generated = record
			}
		}
		require.NotEmpty(t, generated.ID)
		seed := seedRecordingEvalOccurrence(t, f, manifest, generated, "generated-only", "generated-only", "generated-only")
		server := f.server(true)
		server.messageRecordings.SearchProfile = manifest.Profile
		if mode != "lexical" {
			require.NoError(t, recordingEvalControl(t.Context(), manifest, "arm", ""))
			defer func() {
				require.NoError(t, recordingEvalControl(context.WithoutCancel(t.Context()), manifest, "release", ""))
			}()
		}
		response := requireRecordingEvalSearch(t, server, "telescope", mode)
		if mode == "semantic" {
			assert.Empty(t, response.Results)
			assert.Equal(t, "unavailable", response.ActualMode)
		} else {
			require.Len(t, response.Results, 1)
			assert.Equal(t, seed.seed.messageID, response.Results[0].MessageID)
			assert.Equal(t, "lexical", response.ActualMode)
		}
		if mode != "lexical" {
			assert.True(t, response.Partial)
			assert.Equal(t, "incomplete", response.Coverage.State)
			assert.Zero(t, response.Coverage.CompleteDocuments)
		}
		if mode == "hybrid" {
			assert.Contains(t, response.Degradations, "semantic_unavailable")
		}
	})
	scenario("different-current-caption-even-without-matches", func(t *testing.T) {
		f, server, _ := seedRecordingEvaluation(t, manifest, manifest.Profile)
		record := manifest.Records[0]
		other := record
		other.SuppliedInputID, other.Text = manifest.Records[1].SuppliedInputID, manifest.Records[1].Text
		alternate := seedRecordingEvalOccurrence(t, f, manifest, other, "different-caption", "alternate-caption", "alternate")
		response := requireRecordingEvalSearch(t, server, "unmatchablefixtureword", mode)
		for _, hit := range response.Results {
			if hit.MessageID == alternate.seed.messageID {
				forbiddenResults++
			}
			assert.NotEqual(t, alternate.seed.messageID, hit.MessageID)
		}
		assert.GreaterOrEqual(t, response.AttributionUnavailable, 1)
		assert.True(t, response.Partial)
	})

	for _, action := range []string{"replace-local-bytes", "hide-local", "revoke-local", "change-person-scope"} {
		scenario(action, func(t *testing.T) {
			f, _, occurrences := seedRecordingEvaluation(t, manifest, manifest.Profile)
			selected := occurrences[0]
			survivor := f.message(t, "hide-survivor")
			participant := f.f.EnsureParticipant("scope@example.test", "Scope", "example.test")
			person, _, err := f.f.Store.CreatePersonFromParticipant(participant)
			require.NoError(t, err)
			_, err = f.f.Store.DB().ExecContext(t.Context(), f.f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), participant, selected.seed.messageID)
			require.NoError(t, err)
			var mutated atomic.Bool
			var once sync.Once
			target, err := url.Parse(manifest.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.ModifyResponse = func(response *http.Response) (mutationErr error) {
				if response.Request.URL.Path != "/api/v1/search" {
					return nil
				}
				once.Do(func() {
					switch action {
					case "replace-local-bytes":
						_, mutationErr = f.f.Store.DB().ExecContext(t.Context(), f.f.Store.Rebind(`UPDATE attachments SET content_hash = ? WHERE id = ?`), strings.Repeat("c", 64), selected.seed.attachmentID)
					case "hide-local":
						_, mutationErr = f.f.Store.MergeDuplicates(t.Context(), survivor, []int64{selected.seed.messageID}, "qualification-hide")
					case "revoke-local":
						_, mutationErr = f.f.Store.DB().ExecContext(t.Context(), f.f.Store.Rebind(`UPDATE beeper_media_occurrences SET retention_state = 'revoked' WHERE occurrence_ref = ?`), selected.ref)
					case "change-person-scope":
						_, mutationErr = f.f.Store.DB().ExecContext(t.Context(), f.f.Store.Rebind(`UPDATE messages SET sender_id = NULL WHERE id = ?`), selected.seed.messageID)
					}
					mutated.Store(true)
				})
				return mutationErr
			}
			transport := httptest.NewServer(proxy)
			t.Cleanup(transport.Close)
			proxied := manifest
			proxied.URL = transport.URL
			f.client = recordingEvalClient(t, proxied)
			server := f.server(true)
			server.messageRecordings.SearchProfile = manifest.Profile
			query := "q=telescope&recordings_mode=" + mode
			if action == "change-person-scope" {
				query += fmt.Sprintf("&person_id=%d&direction=from_person", person.ID)
			}
			status, response, raw := searchMediaFor(t, server, query)
			require.Equal(t, http.StatusOK, status, raw)
			assert.True(t, mutated.Load(), "actual remote result must be in flight before mutation")
			for _, hit := range response.Results {
				if hit.MessageID == selected.seed.messageID {
					forbiddenResults++
				}
				assert.NotEqual(t, selected.seed.messageID, hit.MessageID)
			}
			if action != "change-person-scope" {
				var live []int64
				for _, hit := range response.Results {
					live = append(live, hit.MessageID)
				}
				assert.Contains(t, live, occurrences[1].seed.messageID, "the other live occurrence of shared bytes must survive")
			}
			assert.True(t, response.Partial)
		})
	}

	for _, mutation := range []struct{ action, id string }{{"replace", "a-v1"}, {"hide", "a-v2"}, {"revoke", "b-v1"}} {
		scenario("source-"+mutation.action, func(t *testing.T) {
			profile := manifest.Profile
			for _, record := range manifest.Records {
				if record.ID == mutation.id {
					profile = record.Profile
				}
			}
			_, server, occurrences := seedRecordingEvaluation(t, manifest, profile)
			var response MediaSearchResponse
			if mode == "lexical" {
				require.NoError(t, recordingEvalControl(t.Context(), manifest, mutation.action, mutation.id))
				status, report, raw := searchMediaFor(t, server, "q=telescope&recordings_mode="+mode)
				observedStatus, response = status, report
				require.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, status, raw)
			} else {
				require.NoError(t, recordingEvalControl(t.Context(), manifest, "arm", ""))
				t.Cleanup(func() { _ = recordingEvalControl(context.WithoutCancel(t.Context()), manifest, "release", "") })
				type outcome struct {
					code   int
					report MediaSearchResponse
					raw    string
				}
				done := make(chan outcome, 1)
				go func() {
					request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/media/search?q=telescope&recordings_mode="+mode, nil)
					request.Header.Set("X-Api-Key", recordingsTestAPIKey)
					writer := httptest.NewRecorder()
					server.Router().ServeHTTP(writer, request)
					var report MediaSearchResponse
					_ = json.Unmarshal(writer.Body.Bytes(), &report)
					done <- outcome{writer.Code, report, writer.Body.String()}
				}()
				require.NoError(t, recordingEvalControl(t.Context(), manifest, "wait", ""))
				require.NoError(t, recordingEvalControl(t.Context(), manifest, mutation.action, mutation.id))
				require.NoError(t, recordingEvalControl(t.Context(), manifest, "release", ""))
				select {
				case result := <-done:
					require.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, result.code, result.raw)
					observedStatus, response = result.code, result.report
				case <-t.Context().Done():
					require.NoError(t, t.Context().Err())
				}
			}
			for _, occurrence := range occurrences {
				if occurrence.record.ID == mutation.id {
					for _, hit := range response.Results {
						if hit.MessageID == occurrence.seed.messageID {
							forbiddenResults++
						}
						assert.NotEqual(t, occurrence.seed.messageID, hit.MessageID, "forbidden stale source result")
					}
				}
			}
			if observedStatus == http.StatusOK {
				assert.True(t, response.Partial)
			} else {
				assert.Empty(t, response.Results)
			}
			require.NoError(t, recordingEvalControl(t.Context(), manifest, "refresh", ""))
		})
	}
	scenario("unreachable", func(t *testing.T) {
		f, _, _ := seedRecordingEvaluation(t, manifest, manifest.Profile)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		unavailable := manifest
		unavailable.URL = "http://" + listener.Addr().String()
		require.NoError(t, listener.Close())
		f.client = recordingEvalClient(t, unavailable)
		server := f.server(true)
		server.messageRecordings.SearchProfile = manifest.Profile
		status, _, raw := searchMediaFor(t, server, "q=telescope&recordings_mode="+mode)
		assert.Equal(t, http.StatusServiceUnavailable, status, raw)
	})
}

func recordingEvalClient(t *testing.T, manifest recordingEvalManifest) *docbankmedia.Client {
	t.Helper()
	client, err := docbankmedia.NewClient(manifest.URL, func() (string, error) { return manifest.APIKey, nil })
	require.NoError(t, err)
	return client
}

func requireRecordingEvalSearch(t *testing.T, server *Server, query, mode string) MediaSearchResponse {
	t.Helper()
	status, response, raw := searchMediaFor(t, server, "q="+url.QueryEscape(query)+"&recordings_mode="+mode+"&limit=100")
	require.Equal(t, http.StatusOK, status, raw)
	return response
}

func recordingEvalControl(ctx context.Context, manifest recordingEvalManifest, action, id string) error {
	raw, err := json.Marshal(map[string]string{"action": action, "id": id})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, manifest.URL+"/qualification/control", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("X-Api-Key", manifest.APIKey)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 25 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("fixture control %s: status %d: %s", action, response.StatusCode, body)
	}
	return nil
}

func seedRecordingEvaluation(t *testing.T, manifest recordingEvalManifest, profile string) (*recordingFixture, *Server, []recordingEvalOccurrence) {
	t.Helper()
	f := newRecordingFixture(t)
	f.client = recordingEvalClient(t, manifest)
	var mailbox struct {
		Messages []struct {
			MessageID      string `json:"message_id"`
			ConversationID string `json:"conversation_id"`
			Body           string `json:"body"`
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "eval", "testdata", "media", "mailbox.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &mailbox))
	require.Len(t, mailbox.Messages, 5)
	records := make(map[string]recordingEvalRecord)
	for _, record := range manifest.Records {
		require.NotContains(t, records, record.ID)
		require.Len(t, record.SourceSHA256, 64)
		require.Positive(t, record.ByteLength)
		records[record.ID] = record
	}
	var occurrences []recordingEvalOccurrence
	for i, message := range mailbox.Messages {
		id := strings.TrimPrefix(message.ConversationID, "media-")
		record, exists := records[id]
		require.True(t, exists)
		require.Equal(t, message.Body, record.Text)
		occurrences = append(occurrences, seedRecordingEvalOccurrence(t, f, manifest, record, message.MessageID, message.ConversationID, fmt.Sprintf("eval-%d", i)))
	}
	server := f.server(true)
	server.messageRecordings.SearchProfile = profile
	return f, server, occurrences
}

func seedRecordingEvalOccurrence(t *testing.T, f *recordingFixture, manifest recordingEvalManifest, record recordingEvalRecord, messageKey, conversationKey, sourceMessageID string) recordingEvalOccurrence {
	t.Helper()
	conversationID, err := f.f.Store.EnsureConversation(t.Context(), f.f.Source.ID, conversationKey, "Synthetic recording")
	require.NoError(t, err)
	f.f.ConvID = conversationID
	if record.Origin == "supplied" {
		f.transcripts[sourceMessageID] = record.Text
	}
	messageID := f.message(t, sourceMessageID)
	partKey := "beeper:mxc://audio/" + sourceMessageID
	require.NoError(t, f.f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: record.ID + ".wav", MIMEType: "audio/wav", StoragePath: record.SourceSHA256[:2] + "/" + record.SourceSHA256,
		ContentHash: record.SourceSHA256, Size: record.ByteLength, SourceAttachmentID: partKey, SourcePartKey: partKey,
		MediaType: "voice_note", State: attachmentpolicy.StateStored, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
	}))
	var attachmentID int64
	require.NoError(t, f.f.Store.DB().QueryRowContext(t.Context(), f.f.Store.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentID))
	revision, err := beeper.MediaRevision(t.Context(), f.f.Store, attachmentID)
	require.NoError(t, err)
	rawDigest := sha256.Sum256(f.writeRaw(t, messageID, sourceMessageID))
	ref := "msgvault:" + sourceMessageID
	processingKey := "eval:" + record.ID + ":" + record.SuppliedInputID
	transcriptHash := ""
	if record.Origin == "supplied" {
		digest := sha256.Sum256([]byte(record.Text))
		transcriptHash = hex.EncodeToString(digest[:])
	}
	occurrenceJSON, err := json.Marshal(map[string]string{"ref": ref, "revision": revision})
	require.NoError(t, err)
	require.NoError(t, f.f.Store.ReconcileBeeperMediaMapping(t.Context(), store.BeeperMediaMapping{
		DestinationKey: recordingsTestDestination, OccurrenceRef: ref, Revision: revision, SourceType: "beeper", SourceIdentifier: "signal",
		SourceConversationID: conversationKey, SourceMessageID: sourceMessageID, SourceAttachmentID: partKey, SourcePartKey: partKey,
		MessageID: messageID, AttachmentID: attachmentID, SourceSHA256: record.SourceSHA256, ByteLength: record.ByteLength,
		RawHash: hex.EncodeToString(rawDigest[:]), TranscriptSHA256: transcriptHash, OccurrenceJSON: string(occurrenceJSON), Filename: record.ID + ".wav", MIMEType: "audio/wav",
		ProcessingKey: processingKey, ProcessingProvider: "beeper", ProcessingProfile: record.Profile,
	}))
	_, err = f.f.Store.DB().ExecContext(t.Context(), f.f.Store.Rebind(`UPDATE beeper_media_deliveries SET supplied_input_id = ? WHERE destination_key = ? AND processing_key = ?`), record.SuppliedInputID, recordingsTestDestination, processingKey)
	require.NoError(t, err)
	operation, err := f.f.Store.PrepareBeeperMediaOperation(t.Context(), store.BeeperMediaOperation{Kind: store.BeeperMediaOperationRetain, DestinationKey: recordingsTestDestination, OccurrenceRef: ref, Revision: revision})
	require.NoError(t, err)
	applied, err := f.f.Store.FinishBeeperMediaOperation(t.Context(), operation, store.BeeperMediaResult{VaultUID: manifest.VaultUID, DocbankSourceID: record.SourceID, SourceVersionID: record.SourceVersionID, ContentVersionID: record.ContentVersionID, DocbankOccurrenceID: "fixture-" + sourceMessageID, CoverageState: "transcribed"})
	require.NoError(t, err)
	require.True(t, applied)
	f.deliver(t, processingKey, "succeeded")
	return recordingEvalOccurrence{record: record, seed: recordingSeed{messageID: messageID, attachmentID: attachmentID, sourceID: record.SourceID, contentVersionID: record.ContentVersionID, suppliedInputID: record.SuppliedInputID}, messageKey: messageKey, conversationKey: conversationKey, ref: ref}
}
