package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

// Lane and consent states shared by `setup providers` and `setup status`.
const (
	laneStateOn      = "on"
	laneStateOff     = "off"
	laneStatePending = "pending"

	consentActive  = "active"
	consentMissing = "missing"
	consentUnknown = "unknown"

	laneTextSearch      = "text_search"
	lanePersonSearch    = "person_search"
	laneVisualSearch    = "visual_search"
	laneDocuments       = "documents"
	laneDocumentVectors = "document_vectors"
	lanePeopleInference = "people_inference"
	laneActivity        = "activity"
	laneMediaPolicy     = "media_policy"

	// Recommended provider defaults. These are the values setup writes when
	// nothing is configured; every one of them remains settable per lane.
	setupVoyageKeyEnv       = "VOYAGE_API_KEY" // #nosec G101 -- environment variable name, not a credential.
	setupOpenAIKeyEnv       = "OPENAI_API_KEY" // #nosec G101 -- environment variable name, not a credential.
	setupVoyageEndpoint     = "https://api.voyageai.com/v1"
	setupVoyageTextModel    = "voyage-context-4"
	setupVoyageTextDim      = 1024
	setupOpenAIEndpoint     = "https://api.openai.com/v1"
	setupOpenAITextModel    = "text-embedding-3-small"
	setupOpenAITextDim      = 1536
	setupOllamaTextModel    = "nomic-embed-text"
	setupOllamaTextDim      = 768
	setupOllamaDocPrefix    = "search_document: "
	setupOllamaQueryPrefix  = "search_query: "
	setupOllamaMaxInput     = 2000
	setupEmbedCron          = "*/15 * * * *"
	setupInferenceModel     = "gpt-5.6-luna"
	setupInferenceReasoning = "medium"
	setupInferenceProfile   = "openai"
	setupOllamaProfile      = "ollama"
	setupPostureDeclared    = "provider-declared"

	setupVoyageManifestName  = "voyage-capabilities.json"
	setupMistralManifestName = "mistral-capabilities.json"
)

// laneStatus is one row of the provider report. It answers, for one lane:
// which provider and model, whether it is on, why not, and what to run next.
type laneStatus struct {
	Lane            string            `json:"lane"`
	Label           string            `json:"label"`
	State           string            `json:"state"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	Schedule        string            `json:"schedule,omitempty"`
	Consent         string            `json:"consent,omitempty"`
	ConsentPurposes map[string]string `json:"consent_purposes,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Next            []string          `json:"next,omitempty"`
}

// laneReport is the complete report printed by both setup subcommands.
type laneReport struct {
	ConfigPath string       `json:"config_path"`
	Lanes      []laneStatus `json:"lanes"`
	MCPTools   []string     `json:"mcp_tools_live"`
}

// setupConsentState is a best-effort view of recorded consents. A nil
// pointer means the archive could not be read (missing database, daemon
// incompatibility, PostgreSQL unreachable) and every consent is reported as
// unknown rather than missing.
type setupConsentState struct {
	Documents         bool
	Visual            bool
	PersonInference   bool
	PersonSemantic    bool
	DocumentEmbedding bool
	QueryEmbedding    bool
}

// setupEnvironment is what the report needs beyond the loaded config: the
// process environment, the filesystem, and the archive's consent records.
type setupEnvironment struct {
	lookupEnv  func(string) (string, bool)
	fileExists func(string) bool
	consent    *setupConsentState
}

func (e setupEnvironment) hasEnv(name string) bool {
	if e.lookupEnv == nil || name == "" {
		return false
	}
	value, ok := e.lookupEnv(name)
	return ok && strings.TrimSpace(value) != ""
}

func (e setupEnvironment) reportMissingCredential(lane *laneStatus, name string) {
	if name != "" && !e.hasEnv(name) {
		lane.State = laneStatePending
		lane.Reason += "; environment variable " + name + " is not set"
	}
}

func (e setupEnvironment) exists(path string) bool {
	if e.fileExists == nil || path == "" {
		return false
	}
	return e.fileExists(path)
}

func (e setupEnvironment) consentState(read func(setupConsentState) bool) string {
	if e.consent == nil {
		return consentUnknown
	}
	if read(*e.consent) {
		return consentActive
	}
	return consentMissing
}

func defaultFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// setupVoyageManifestPath is the path setup recommends for the Voyage
// capability manifest, so a re-run can enable the visual lane once the probe
// has written it.
func setupVoyageManifestPath(cfg *config.Config) string {
	return filepath.Join(cfg.HomeDir, setupVoyageManifestName)
}

// setupMistralManifestPath is the recommended Mistral capability manifest path.
func setupMistralManifestPath(cfg *config.Config) string {
	return filepath.Join(cfg.HomeDir, setupMistralManifestName)
}

// readSetupConsentState opens the archive read-only and reads every consent
// the report shows. Failures return nil so a report never blocks on the
// archive; the caller renders those consents as unknown.
func readSetupConsentState(ctx context.Context, cfg *config.Config) *setupConsentState {
	if cfg == nil {
		return nil
	}
	st, err := store.OpenReadOnly(cfg.DatabaseDSN())
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()
	return setupConsentFromStore(ctx, cfg, st)
}

// setupConsentFromStore reads the consent records behind the report from an
// already-open store. Each lookup is independent so one failing table cannot
// hide the others.
func setupConsentFromStore(ctx context.Context, cfg *config.Config, st *store.Store) *setupConsentState {
	state := &setupConsentState{}
	if cfg.Vector.Enabled && cfg.Attachments.Documents.Index.Embeddings.Enabled {
		if target, err := st.GetDocumentVectorTargetProfileID(ctx); err == nil {
			if fingerprint, err := vectordocument.EgressFingerprint(target, cfg.Vector); err == nil {
				consent, err := st.GetDocumentVectorConsent(ctx, fingerprint)
				state.DocumentEmbedding = err == nil && consent != nil && consent.Purpose == "document_embedding"
			}
			if fingerprint, err := vectordocument.QueryEgressFingerprint(target, cfg.Vector); err == nil {
				consent, err := st.GetDocumentVectorConsent(ctx, fingerprint)
				state.QueryEmbedding = err == nil && consent != nil && consent.Purpose == "query_embedding"
			}
		}
	}
	if consented, err := st.HasActiveDocumentProviderConsent(ctx); err == nil {
		state.Documents = consented
	}
	if generation, err := st.ActiveVisualGeneration(ctx); err == nil && generation.Consented {
		state.Visual = true
	} else if generation, err := st.BuildingVisualGeneration(ctx); err == nil && generation.Consented {
		state.Visual = true
	}
	if cfg.People.Sweep.Enabled {
		if profile, err := cfg.People.Sweep.Profile(); err == nil {
			if active, err := st.HasActivePersonInferenceConsent(ctx, profile.Fingerprint); err == nil {
				state.PersonInference = active
			}
		}
	}
	if cfg.Vector.Enabled && cfg.Vector.People.Enabled {
		if profile, err := cfg.Vector.SemanticPersonEmbeddingProfile(); err == nil {
			if active, err := st.HasActivePersonSemanticEmbeddingConsent(ctx, profile.Fingerprint); err == nil {
				state.PersonSemantic = active
			}
		}
	}
	return state
}

// embeddingProviderName names the embedding destination for the report.
func embeddingProviderName(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "custom"
	}
	host := strings.ToLower(parsed.Hostname())
	address, _ := netip.ParseAddr(host)
	if host == "localhost" || address.Unmap().IsLoopback() {
		return "local"
	}
	if parsed.Scheme == "https" && (parsed.Port() == "" || parsed.Port() == "443") {
		switch host {
		case "api.voyageai.com":
			return "voyage"
		case "api.openai.com":
			return "openai"
		}
	}
	return "custom"
}

func embedScheduleSummary(schedule vector.EmbedScheduleConfig) string {
	parts := []string{}
	if schedule.Cron != "" {
		parts = append(parts, "cron "+schedule.Cron)
	}
	if schedule.RunAfterSync {
		parts = append(parts, "after each scheduled sync")
	}
	if len(parts) == 0 {
		return "manual only"
	}
	return strings.Join(parts, ", ")
}

// buildLaneReport derives the lane report from one loaded config plus the
// environment. It never contacts a provider.
func buildLaneReport(cfg *config.Config, env setupEnvironment) laneReport {
	report := laneReport{ConfigPath: cfg.ConfigFilePath()}
	report.Lanes = append(report.Lanes,
		textSearchLane(cfg, env),
		personSearchLane(cfg, env),
		visualSearchLane(cfg, env),
		documentsLane(cfg, env),
		documentVectorsLane(cfg, env),
		peopleInferenceLane(cfg, env),
		activityLane(cfg),
		mediaPolicyLane(cfg),
	)
	report.MCPTools = liveMCPTools(cfg)
	return report
}

func textSearchLane(cfg *config.Config, env setupEnvironment) laneStatus {
	lane := laneStatus{Lane: laneTextSearch, Label: "Text search (messages, chats, meetings)"}
	embeddings := cfg.Vector.Embeddings
	if cfg.Vector.Enabled {
		lane.State = laneStateOn
		lane.Provider = embeddingProviderName(embeddings.Endpoint)
		lane.Model = embeddings.Model
		lane.Schedule = embedScheduleSummary(cfg.Vector.Embed.Schedule)
		if embeddings.EffectiveAPIFormat() == vector.APIFormatVoyageContextual {
			lane.Reason = "conversation windows and turn-aware meeting chunks share one contextual generation"
		} else {
			lane.Reason = "per-message vectors; no conversation-window context (Voyage contextual only)"
		}
		env.reportMissingCredential(&lane, embeddings.APIKeyEnv)
		return lane
	}
	lane.State = laneStateOff
	switch {
	case embeddings.Endpoint != "" || embeddings.Model != "":
		lane.Reason = "configured but disabled; set [vector] enabled = true"
	case env.hasEnv(setupVoyageKeyEnv) || env.hasEnv(setupOpenAIKeyEnv):
		lane.State = laneStatePending
		lane.Reason = "an embedding key is present but the lane is not configured"
		lane.Next = []string{"msgvault setup providers"}
	default:
		lane.Reason = "no embedding provider; set " + setupVoyageKeyEnv + " (recommended) or " +
			setupOpenAIKeyEnv + ", or run Ollama with " + setupOllamaTextModel + ", then run `msgvault setup providers`"
	}
	return lane
}

func personSearchLane(cfg *config.Config, env setupEnvironment) laneStatus {
	lane := laneStatus{Lane: lanePersonSearch, Label: "Semantic people search"}
	switch {
	case cfg.Vector.Enabled && cfg.Vector.People.Enabled:
		lane.State = laneStateOn
		lane.Provider = embeddingProviderName(cfg.Vector.Embeddings.Endpoint)
		lane.Model = cfg.Vector.Embeddings.Model
		lane.Consent = env.consentState(func(s setupConsentState) bool { return s.PersonSemantic })
		lane.Reason = "one curated document per person rides the text-search generation"
		if text := textSearchLane(cfg, env); text.State != laneStateOn {
			lane.State = laneStatePending
			lane.Reason += "; text search is not ready: " + text.Reason
		}
		if lane.Consent != consentActive {
			lane.State = laneStatePending
			lane.Next = []string{"msgvault person provider consent --semantic-embeddings --yes"}
		}
	case cfg.Vector.Enabled:
		lane.State = laneStateOff
		lane.Reason = "set [vector.people] enabled = true with explicit retention and training postures"
	default:
		lane.State = laneStateOff
		lane.Reason = "requires the text-search lane"
	}
	return lane
}

func visualSearchLane(cfg *config.Config, env setupEnvironment) laneStatus {
	lane := laneStatus{Lane: laneVisualSearch, Label: "Visual attachment search"}
	multimodal := cfg.Vector.Multimodal
	if multimodal.Enabled {
		lane.State = laneStateOn
		lane.Provider = multimodal.Provider
		lane.Model = multimodal.Model
		lane.Schedule = embedScheduleSummary(multimodal.Schedule)
		lane.Consent = env.consentState(func(s setupConsentState) bool { return s.Visual })
		if !env.exists(multimodal.CapabilitiesFile) {
			lane.State = laneStatePending
			lane.Reason = "capabilities_file is missing; the daemon refuses every vector lane until it exists"
			lane.Next = []string{visualProbeCommand(cfg)}
			env.reportMissingCredential(&lane, multimodal.APIKeyEnv)
			return lane
		}
		lane.Reason = "eligible images and short videos are embedded with bounded message context"
		if lane.Consent != consentActive {
			lane.State = laneStatePending
			lane.Next = []string{"msgvault multimodal build --yes"}
		}
		env.reportMissingCredential(&lane, multimodal.APIKeyEnv)
		return lane
	}
	lane.State = laneStateOff
	if !env.hasEnv(multimodal.APIKeyEnv) {
		lane.Reason = "needs " + multimodal.APIKeyEnv + " (Voyage is the only visual provider)"
		return lane
	}
	lane.State = laneStatePending
	if env.exists(setupVoyageManifestPath(cfg)) {
		lane.Reason = "probe manifest found; setup can enable the lane"
		lane.Next = []string{"msgvault setup providers"}
		return lane
	}
	lane.Reason = "key present; the provider probe needs private synthetic WebP and MP4 seeds before uploads are authorized"
	lane.Next = []string{visualProbeCommand(cfg), "msgvault setup providers"}
	return lane
}

func visualProbeCommand(cfg *config.Config) string {
	return "msgvault multimodal probe --seeds <private-seed-dir> --out " + setupVoyageManifestPath(cfg) + " --yes"
}

func documentsLane(cfg *config.Config, env setupEnvironment) laneStatus {
	lane := laneStatus{Lane: laneDocuments, Label: "Document attachments (extraction and lexical search)"}
	documents := cfg.Attachments.Documents
	manifest := setupMistralManifestPath(cfg)
	if documents.Enabled {
		lane.State = laneStateOn
		lane.Provider = documents.Provider
		lane.Model = documents.Model
		lane.Consent = env.consentState(func(s setupConsentState) bool { return s.Documents })
		lane.Reason = fmt.Sprintf("region %s; retention=%s, training=%s; uploads are manual-only",
			documents.Region, documents.RetentionPosture, documents.TrainingPosture)
		if lane.Consent != consentActive {
			lane.State = laneStatePending
			if env.exists(manifest) {
				lane.Next = []string{
					"msgvault documents consent-mistral --capabilities " + manifest + " --yes",
					"msgvault documents build --capabilities " + manifest + " --yes",
				}
			} else {
				lane.Next = []string{
					"msgvault documents probe-mistral --fixtures <private-fixture-dir> > " + manifest,
					"msgvault documents consent-mistral --capabilities " + manifest + " --yes",
				}
			}
		}
		env.reportMissingCredential(&lane, documents.APIKeyEnv)
		return lane
	}
	lane.State = laneStateOff
	if env.hasEnv(documents.APIKeyEnv) {
		lane.State = laneStatePending
		lane.Reason = "key present but the lane is not configured"
		lane.Next = []string{"msgvault setup providers"}
		return lane
	}
	lane.Reason = "needs " + documents.APIKeyEnv + " (Mistral is the only document provider)"
	return lane
}

func documentVectorsLane(cfg *config.Config, env setupEnvironment) laneStatus {
	lane := laneStatus{Lane: laneDocumentVectors, Label: "Document semantic search"}
	documents := cfg.Attachments.Documents
	switch {
	case documents.Enabled && documents.Index.Embeddings.Enabled:
		lane.State = laneStateOn
		lane.Provider = embeddingProviderName(cfg.Vector.Embeddings.Endpoint)
		lane.Model = cfg.Vector.Embeddings.Model
		lane.Schedule = embedScheduleSummary(cfg.Vector.Embed.Schedule)
		lane.Reason = "document chunks and search query text are sent to the text-search provider under separate consents"
		if text := textSearchLane(cfg, env); text.State != laneStateOn {
			lane.State = laneStatePending
			lane.Reason += "; text search is not ready: " + text.Reason
		}
		lane.ConsentPurposes = map[string]string{
			"document_embedding": env.consentState(func(s setupConsentState) bool { return s.DocumentEmbedding }),
			"query_embedding":    env.consentState(func(s setupConsentState) bool { return s.QueryEmbedding }),
		}
		lane.Consent = env.consentState(func(s setupConsentState) bool { return s.DocumentEmbedding && s.QueryEmbedding })
		if lane.Consent != consentActive {
			lane.State = laneStatePending
		}
		if lane.ConsentPurposes["document_embedding"] != consentActive {
			lane.Next = append(lane.Next, "msgvault documents vectors consent --yes")
		}
		if lane.ConsentPurposes["query_embedding"] != consentActive {
			lane.Next = append(lane.Next, "msgvault documents vectors consent --purpose queries --yes")
		}
	case documents.Enabled && !cfg.Vector.Enabled:
		lane.State = laneStateOff
		lane.Reason = "requires the text-search lane"
	case documents.Enabled:
		lane.State = laneStateOff
		lane.Reason = "set [attachments.documents.index.embeddings] enabled = true"
	default:
		lane.State = laneStateOff
		lane.Reason = "requires the document lane"
	}
	return lane
}

func peopleInferenceLane(cfg *config.Config, env setupEnvironment) laneStatus {
	lane := laneStatus{Lane: lanePeopleInference, Label: "People sweep (attribute maintenance)"}
	sweep := cfg.People.Sweep
	if sweep.Enabled {
		lane.State = laneStateOn
		name, provider, err := sweep.ActiveProviderConfig()
		if err == nil {
			lane.Provider = name + " (" + string(provider.Protocol) + ")"
			lane.Model = provider.Model
		}
		lane.Schedule = "cron " + sweep.Schedule
		lane.Consent = env.consentState(func(s setupConsentState) bool { return s.PersonInference })
		lane.Reason = "runs for tracked people only; deterministic contact state refreshes for everyone through the activity job"
		if lane.Consent != consentActive {
			lane.State = laneStatePending
			if name != "" {
				lane.Next = []string{"msgvault person provider consent " + name + " --yes"}
			}
		}
		lane.Next = append(lane.Next, "msgvault person track <person-id>")
		if err == nil && provider.Auth != peoplesweep.AuthNone && provider.Credential == peoplesweep.CredentialEnv {
			env.reportMissingCredential(&lane, provider.CredentialEnv)
		}
		return lane
	}
	lane.State = laneStateOff
	if env.hasEnv(setupOpenAIKeyEnv) {
		lane.State = laneStatePending
		lane.Reason = setupOpenAIKeyEnv + " present; setup can onboard the " + setupInferenceModel + " profile"
		lane.Next = []string{"msgvault setup providers"}
		return lane
	}
	lane.Reason = "needs " + setupOpenAIKeyEnv + " or a reachable local Ollama server, then `msgvault setup providers`"
	return lane
}

func activityLane(cfg *config.Config) laneStatus {
	lane := laneStatus{Lane: laneActivity, Label: "Contact activity (last contacted, cadence)"}
	if cfg.Activity.Schedule == "" {
		lane.State = laneStateOff
		lane.Reason = "[activity] schedule is empty; run `msgvault activity build` by hand"
		return lane
	}
	lane.State = laneStateOn
	lane.Schedule = "cron " + cfg.Activity.Schedule
	lane.Reason = "projects archived messages into dated per-person contact state (" + cfg.Activity.Timezone + ")"
	return lane
}

func mediaPolicyLane(cfg *config.Config) laneStatus {
	lane := laneStatus{Lane: laneMediaPolicy, Label: "Chat media collection", State: laneStateOn}
	summaries := []string{
		"beeper " + mediaPolicySummary(cfg.Beeper.MediaPolicy("")),
		"slack " + mediaPolicySummary(cfg.Slack.MediaPolicy("")),
		"discord " + mediaPolicySummary(cfg.Discord.MediaPolicy("")),
		"teams " + mediaPolicySummary(cfg.Teams.MediaPolicy("")),
	}
	lane.Reason = strings.Join(summaries, "; ")
	return lane
}

func mediaPolicySummary(policy attachmentpolicy.Policy) string {
	if policy.DisabledReason != "" {
		return "off"
	}
	participants := "any size room"
	if policy.MaxParticipants > 0 {
		participants = fmt.Sprintf("rooms up to %d participants", policy.MaxParticipants)
	}
	size := "no size cap"
	if policy.MaxBytes > 0 {
		size = fmt.Sprintf("%d MiB cap", policy.MaxBytes>>20)
	}
	return fmt.Sprintf("scope %s, %s, %s", policy.Scope, participants, size)
}

func liveMCPTools(cfg *config.Config) []string {
	tools := []string{"search_people", "get_person_notes", "get_person_relationship", "search_person_files"}
	if cfg.Vector.Enabled {
		tools = append(tools, "semantic_search_messages", "find_similar_messages")
	}
	if cfg.Vector.Multimodal.Enabled {
		tools = append(tools, "search_visual_attachments")
	}
	if cfg.Attachments.Documents.Enabled {
		tools = append(tools, "search_document_attachments")
	}
	return tools
}

func writeLaneReport(w io.Writer, report laneReport, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "LANE\tSTATE\tPROVIDER\tMODEL\tCONSENT\tSCHEDULE")
	for _, lane := range report.Lanes {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			lane.Label, lane.State, dash(lane.Provider), dash(lane.Model), dash(lane.Consent), dash(lane.Schedule))
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write lane report: %w", err)
	}
	_, _ = fmt.Fprintln(w)
	for _, lane := range report.Lanes {
		if lane.Reason == "" && len(lane.Next) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "%s: %s\n", lane.Label, lane.Reason)
		for _, purpose := range slices.Sorted(maps.Keys(lane.ConsentPurposes)) {
			_, _ = fmt.Fprintf(w, "  %s consent: %s\n", purpose, lane.ConsentPurposes[purpose])
		}
		for _, next := range lane.Next {
			_, _ = fmt.Fprintf(w, "  next: %s\n", next)
		}
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "MCP tools live with this configuration: %s\n", strings.Join(report.MCPTools, ", "))
	_, _ = fmt.Fprintf(w, "Config: %s\n", report.ConfigPath)
	return nil
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
