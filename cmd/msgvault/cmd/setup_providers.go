package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

const (
	planActionEnable  = "enable"
	planActionKeep    = "keep"
	planActionPending = "pending"
	planActionSkip    = "skip"
	planActionOnboard = "onboard"

	// Consent gates: one explicit answer per hosted provider.
	gateVoyage  = "voyage"
	gateMistral = "mistral"
	gateOpenAI  = "openai"

	ollamaProbeTimeout = 2 * time.Second
	ollamaProbeMaxBody = 1 << 20

	tomlTableVector = "vector"
)

// setupProvidersOptions are the command flags.
type setupProvidersOptions struct {
	yes                    bool
	dryRun                 bool
	jsonOutput             bool
	allowSensitive         bool
	documentRetention      string
	documentTraining       string
	retentionPosture       string
	trainingPosture        string
	personRetentionPosture string
	personTrainingPosture  string
}

// ollamaProbeResult is what a local Ollama server reports about itself.
type ollamaProbeResult struct {
	Reachable bool
	Models    []string
}

// setupProvidersDeps isolates the pass from the process for tests: the
// environment, the config file, the archive, the daemon, and the people
// provider onboarding machinery are all injectable.
type setupProvidersDeps struct {
	lookupEnv         func(string) (string, bool)
	fileExists        func(string) bool
	readConfigFile    func() (config.ConfigFile, error)
	editConfigTables  func(string, []config.TableEdit) (config.ConfigFile, error)
	restoreConfigFile func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error)
	loadConfig        func(config.ConfigFile) (*config.Config, error)
	remoteConfigured  func() bool
	isTerminal        func(*cobra.Command) bool
	probeOllama       func(context.Context, string) ollamaProbeResult
	consentState      func(context.Context, *config.Config) *setupConsentState
	daemonAlive       func(context.Context, *config.Config) bool
	personProvider    func() personProviderCommandDeps
	now               func() time.Time
}

func defaultSetupProvidersDeps() setupProvidersDeps {
	return setupProvidersDeps{
		lookupEnv:  os.LookupEnv,
		fileExists: defaultFileExists,
		readConfigFile: func() (config.ConfigFile, error) {
			if cfg == nil {
				return config.ConfigFile{}, errors.New("configuration is unavailable")
			}
			return config.ReadConfigFile(cfg.ConfigFilePath())
		},
		editConfigTables: func(ifMatch string, edits []config.TableEdit) (config.ConfigFile, error) {
			if cfg == nil {
				return config.ConfigFile{}, errors.New("configuration is unavailable")
			}
			return config.EditConfigTables(cfg.ConfigFilePath(), ifMatch, edits)
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			return config.RestoreConfigFile(before.LogicalPath, published, before)
		},
		loadConfig: func(snapshot config.ConfigFile) (*config.Config, error) {
			if cfg == nil {
				return nil, errors.New("configuration is unavailable")
			}
			return loadSetupConfig(snapshot, cfg.HomeDir)
		},
		remoteConfigured: IsRemoteMode,
		isTerminal:       commandStdinIsTerminal,
		probeOllama:      probeOllamaServer,
		consentState:     readSetupConsentState,
		daemonAlive: func(ctx context.Context, loaded *config.Config) bool {
			return findAnyDaemonRuntimeContext(ctx, loaded.Data.DataDir) != nil
		},
		personProvider: defaultPersonProviderCommandDeps,
		now:            time.Now,
	}
}

// loadSetupConfig decodes a snapshot the way the daemon would, and keeps the
// operator's home directory when the file does not exist yet so recommended
// manifest paths land beside the config that setup is about to create.
func loadSetupConfig(snapshot config.ConfigFile, homeDir string) (*config.Config, error) {
	loaded, err := config.LoadConfigFile(snapshot, homeDir)
	if err != nil {
		return nil, err
	}
	if !snapshot.Exists && homeDir != "" {
		loaded.HomeDir = homeDir
		loaded.Data.DataDir = homeDir
	}
	return loaded, nil
}

func commandStdinIsTerminal(command *cobra.Command) bool {
	file, ok := command.InOrStdin().(*os.File)
	return ok && term.IsTerminal(file.Fd())
}

// probeOllamaServer lists the models a local Ollama server exposes. It is
// consulted only when no hosted embedding key is present; a failure simply
// reports the server as unreachable.
func probeOllamaServer(ctx context.Context, server string) ollamaProbeResult {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" {
		return ollamaProbeResult{}
	}
	ctx, cancel := context.WithTimeout(ctx, ollamaProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/tags", nil)
	if err != nil {
		return ollamaProbeResult{}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ollamaProbeResult{}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return ollamaProbeResult{}
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, ollamaProbeMaxBody)).Decode(&payload); err != nil {
		return ollamaProbeResult{Reachable: true}
	}
	result := ollamaProbeResult{Reachable: true}
	for _, model := range payload.Models {
		if name := strings.TrimSpace(model.Name); name != "" {
			result.Models = append(result.Models, name)
		}
	}
	return result
}

func (r ollamaProbeResult) hasModel(name string) bool {
	for _, model := range r.Models {
		if model == name || strings.HasPrefix(model, name+":") {
			return true
		}
	}
	return false
}

// setupDetection is everything the plan is keyed off: which keys exist, what
// the local Ollama server offers, which probe manifests are already written,
// and which vector backend the archive selects.
type setupDetection struct {
	configKeys         toml.MetaData
	voyageKey          bool
	mistralKey         bool
	mistralKeyEnv      string
	openAIKey          bool
	ollama             ollamaProbeResult
	ollamaEndpoint     string
	ollamaLoopback     bool
	voyageManifest     string
	mistralManifest    string
	backend            string
	backendUnavailable string
}

func detectSetupProviders(ctx context.Context, loaded *config.Config, deps setupProvidersDeps) setupDetection {
	env := setupEnvironment{lookupEnv: deps.lookupEnv, fileExists: deps.fileExists}
	detection := setupDetection{
		voyageKey:     env.hasEnv(setupVoyageKeyEnv),
		mistralKeyEnv: loaded.Attachments.Documents.APIKeyEnv,
		openAIKey:     env.hasEnv(setupOpenAIKeyEnv),
	}
	detection.mistralKey = env.hasEnv(detection.mistralKeyEnv)
	detection.backend, detection.backendUnavailable = setupVectorBackend(loaded)
	if path := loaded.Vector.Multimodal.CapabilitiesFile; path != "" && env.exists(path) {
		detection.voyageManifest = path
	} else if path := setupVoyageManifestPath(loaded); env.exists(path) {
		detection.voyageManifest = path
	}
	if path := setupMistralManifestPath(loaded); env.exists(path) {
		detection.mistralManifest = path
	}
	// The local server is consulted only when nothing hosted is available
	// for the lane it would fill, so a key holder never waits on a probe.
	textUnconfigured := !loaded.Vector.Enabled && loaded.Vector.Embeddings.Endpoint == ""
	needsLocalText := textUnconfigured && !detection.voyageKey && !detection.openAIKey
	needsLocalInference := !loaded.People.Sweep.Enabled && !detection.openAIKey
	if (needsLocalText || needsLocalInference) && deps.probeOllama != nil {
		server := strings.TrimRight(strings.TrimSpace(loaded.Chat.Server), "/")
		detection.ollama = deps.probeOllama(ctx, server)
		detection.ollamaEndpoint = server + "/v1"
		detection.ollamaLoopback = embeddingProviderName(server) == "local"
	}
	return detection
}

// setupLanePlan is one lane's decision. The edits and next steps are
// attached so the plan can be printed before anything is written.
type setupLanePlan struct {
	Lane     string `json:"lane"`
	Label    string `json:"label"`
	Action   string `json:"action"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Reason   string `json:"reason"`
	Gate     string `json:"consent_gate,omitempty"`
	edits    []config.TableEdit
	next     []string
}

// setupInferencePlan onboards one people-sweep provider profile through the
// same add, check, consent, and use path the CLI exposes.
type setupInferencePlan struct {
	name    string
	options personProviderAddOptions
	gate    string
}

type setupProvidersPlan struct {
	Lanes     []setupLanePlan
	inference *setupInferencePlan
}

func (p *setupProvidersPlan) laneOn(lane string) bool {
	for _, item := range p.Lanes {
		if item.Lane == lane {
			return item.Action == planActionEnable || item.Action == planActionKeep || item.Action == planActionOnboard
		}
	}
	return false
}

func (p *setupProvidersPlan) gates() []string {
	seen := map[string]bool{}
	for _, lane := range p.Lanes {
		if lane.Gate != "" && (lane.Action == planActionEnable || lane.Action == planActionOnboard) {
			seen[lane.Gate] = true
		}
	}
	ordered := []string{}
	for _, gate := range []string{gateVoyage, gateMistral, gateOpenAI} {
		if seen[gate] {
			ordered = append(ordered, gate)
		}
	}
	return ordered
}

func (p *setupProvidersPlan) writes() bool {
	for _, lane := range p.Lanes {
		if len(lane.edits) > 0 || lane.Action == planActionOnboard {
			return true
		}
	}
	return false
}

// declineGate turns every lane behind a declined gate into a skip.
func (p *setupProvidersPlan) declineGate(gate string) {
	for i := range p.Lanes {
		if p.Lanes[i].Gate == gate && (p.Lanes[i].Action == planActionEnable || p.Lanes[i].Action == planActionOnboard) {
			p.Lanes[i].Action = planActionSkip
			p.Lanes[i].Reason = "declined"
			p.Lanes[i].edits = nil
			p.Lanes[i].next = nil
		}
	}
	if p.inference != nil && p.inference.gate == gate {
		p.inference = nil
	}
}

// Refresh only still-approved dependent lanes. A declined lane must not be
// proposed again, and subsequent disclosures must describe the reduced plan.
func (p *setupProvidersPlan) refreshDependencies(loaded *config.Config, detection setupDetection, options setupProvidersOptions, now time.Time) {
	for i, lane := range p.Lanes {
		if lane.Action != planActionEnable && lane.Action != planActionOnboard {
			continue
		}
		switch lane.Lane {
		case lanePersonSearch:
			p.Lanes[i] = planPersonSearch(loaded, p, options)
		case laneDocumentVectors:
			p.Lanes[i] = planDocumentVectors(loaded, p)
		case lanePeopleInference:
			p.Lanes[i], p.inference = planPeopleInference(loaded, detection, p, options, now)
		}
	}
}

// mergedEdits collapses the plan into one table edit per path so a single
// ETag-guarded write publishes every lane together.
func (p *setupProvidersPlan) mergedEdits() []config.TableEdit {
	var merged []config.TableEdit
	index := map[string]int{}
	for _, lane := range p.Lanes {
		if lane.Action != planActionEnable && lane.Action != planActionPending {
			continue
		}
		for _, edit := range lane.edits {
			key := strings.Join(edit.Path, "\x00")
			if at, ok := index[key]; ok {
				maps.Copy(merged[at].Values, edit.Values)
				continue
			}
			values := make(map[string]any, len(edit.Values))
			maps.Copy(values, edit.Values)
			index[key] = len(merged)
			merged = append(merged, config.TableEdit{Path: edit.Path, Values: values})
		}
	}
	return merged
}

func validateSetupProvidersOptions(options setupProvidersOptions) error {
	if options.documentRetention != documentindex.RetentionStandard && options.documentRetention != documentindex.RetentionZDR {
		return fmt.Errorf("--document-retention must be %q or %q", documentindex.RetentionStandard, documentindex.RetentionZDR)
	}
	if options.documentTraining != documentindex.TrainingDefaultOptOut && options.documentTraining != documentindex.TrainingOptedOut {
		return fmt.Errorf("--document-training must be %q or %q", documentindex.TrainingDefaultOptOut, documentindex.TrainingOptedOut)
	}
	for name, value := range map[string]string{
		"--retention-posture": options.retentionPosture, "--training-posture": options.trainingPosture,
	} {
		if strings.TrimSpace(value) == "" || strings.EqualFold(strings.TrimSpace(value), "unknown") {
			return fmt.Errorf("%s must be an explicit provider assertion", name)
		}
	}
	return nil
}

// planSetupProviders chooses values for every lane that is still unset. It
// never changes a lane that is already on: a configured text lane keeps its
// model even when a different key appears, because switching the embedding
// policy invalidates the index and is the operator's call.
func planSetupProviders(loaded *config.Config, detection setupDetection, options setupProvidersOptions, now time.Time) setupProvidersPlan {
	// Later lanes read the plan so far: people search and document vectors
	// depend on the text lane, and the sweep's evidence sources depend on the
	// document lane, so the lanes are appended in dependency order.
	plan := setupProvidersPlan{Lanes: []setupLanePlan{planTextSearch(loaded, detection)}}
	person := planPersonSearch(loaded, &plan, options)
	visual := planVisualSearch(loaded, detection)
	documents := planDocuments(loaded, detection, options)
	plan.Lanes = append(plan.Lanes, person, visual, documents)
	vectors := planDocumentVectors(loaded, &plan)
	inferenceLane, inference := planPeopleInference(loaded, detection, &plan, options, now)
	plan.Lanes = append(plan.Lanes, vectors, inferenceLane)
	plan.inference = inference
	return plan
}

func textLaneGate(plan *setupProvidersPlan) string {
	for _, lane := range plan.Lanes {
		if lane.Lane == laneTextSearch {
			if lane.Action == planActionKeep {
				return textLaneGateForProvider(lane.Provider)
			}
			return lane.Gate
		}
	}
	return ""
}

func textLaneGateForProvider(provider string) string {
	switch provider {
	case "voyage":
		return gateVoyage
	case "openai":
		return gateOpenAI
	default:
		return ""
	}
}

func setupScheduleEdits(keys toml.MetaData, lane string) []config.TableEdit {
	path := []string{tomlTableVector, lane, "schedule"}
	values := map[string]any{}
	for name, value := range map[string]any{"run_after_sync": true, "cron": setupEmbedCron} {
		if !keys.IsDefined(tomlTableVector, lane, "schedule", name) {
			values[name] = value
		}
	}
	if len(values) == 0 {
		return nil
	}
	return []config.TableEdit{{Path: path, Values: values}}
}

func planTextSearch(loaded *config.Config, detection setupDetection) setupLanePlan {
	lane := setupLanePlan{Lane: laneTextSearch, Label: "Text search"}
	embeddings := loaded.Vector.Embeddings
	switch {
	case loaded.Vector.Enabled:
		lane.Action = planActionKeep
		lane.Provider = embeddingProviderName(embeddings.Endpoint)
		lane.Model = embeddings.Model
		lane.Reason = "already configured; switching the embedding policy requires `msgvault embeddings build --full-rebuild`"
		return lane
	case embeddings.Endpoint != "" || embeddings.Model != "":
		lane.Action = planActionSkip
		lane.Reason = "configured but disabled; set [vector] enabled = true to turn it on"
		return lane
	}
	if detection.backendUnavailable != "" {
		lane.Action, lane.Reason = planActionPending, detection.backendUnavailable
		return lane
	}
	vectorEdit := config.TableEdit{Path: []string{tomlTableVector}, Values: map[string]any{"enabled": true, "backend": detection.backend}}
	switch {
	case detection.voyageKey:
		lane.Action, lane.Provider, lane.Model, lane.Gate = planActionEnable, "voyage", setupVoyageTextModel, gateVoyage
		lane.Reason = "contextual embeddings: chats embed as conversation windows, meetings as turn-aware chunks, email on the same generation"
		lane.edits = append([]config.TableEdit{vectorEdit, {
			Path: []string{tomlTableVector, "embeddings"},
			Values: map[string]any{
				"api_format": "voyage-contextual", "endpoint": setupVoyageEndpoint,
				"api_key_env": setupVoyageKeyEnv, "model": setupVoyageTextModel, "dimension": setupVoyageTextDim,
			},
		}}, setupScheduleEdits(detection.configKeys, "embed")...)
	case detection.openAIKey:
		lane.Action, lane.Provider, lane.Model, lane.Gate = planActionEnable, "openai", setupOpenAITextModel, gateOpenAI
		lane.Reason = "per-message vectors; no conversation-window context and no visual lane, both are Voyage-only"
		lane.edits = append([]config.TableEdit{vectorEdit, {
			Path: []string{tomlTableVector, "embeddings"},
			Values: map[string]any{
				"api_format": "openai", "endpoint": setupOpenAIEndpoint,
				"api_key_env": setupOpenAIKeyEnv, "model": setupOpenAITextModel, "dimension": setupOpenAITextDim,
			},
		}}, setupScheduleEdits(detection.configKeys, "embed")...)
	case detection.ollama.Reachable && !detection.ollamaLoopback:
		// A reachable server that is not on this machine would receive
		// message text without a credential or a disclosure; only the operator
		// may configure that, explicitly, in [vector.embeddings].
		lane.Action = planActionSkip
		lane.Reason = "Ollama at " + detection.ollamaEndpoint + " is not loopback; setup only selects a local server, configure [vector.embeddings] explicitly to send text off this machine"
		return lane
	case detection.ollama.Reachable && detection.ollama.hasModel(setupOllamaTextModel):
		lane.Action, lane.Provider, lane.Model = planActionEnable, "local", setupOllamaTextModel
		lane.Reason = "local Ollama embeddings; message text stays on this machine"
		lane.edits = append([]config.TableEdit{vectorEdit, {
			Path: []string{tomlTableVector, "embeddings"},
			Values: map[string]any{
				"api_format": "openai", "endpoint": detection.ollamaEndpoint,
				"api_key_env": "",
				"model":       setupOllamaTextModel, "dimension": setupOllamaTextDim,
				"document_prefix": setupOllamaDocPrefix, "query_prefix": setupOllamaQueryPrefix,
				"max_input_chars": setupOllamaMaxInput,
			},
		}}, setupScheduleEdits(detection.configKeys, "embed")...)
	case detection.ollama.Reachable:
		lane.Action = planActionSkip
		lane.Reason = "Ollama is reachable but has no " + setupOllamaTextModel + "; run `ollama pull " + setupOllamaTextModel + "` or set " + setupVoyageKeyEnv
		return lane
	default:
		lane.Action = planActionSkip
		lane.Reason = "no embedding provider: set " + setupVoyageKeyEnv + " (recommended) or " + setupOpenAIKeyEnv + ", or run Ollama with " + setupOllamaTextModel
		return lane
	}
	lane.next = []string{"msgvault embeddings build --yes"}
	return lane
}

func planPersonSearch(loaded *config.Config, plan *setupProvidersPlan, options setupProvidersOptions) setupLanePlan {
	lane := setupLanePlan{Lane: lanePersonSearch, Label: "Semantic people search"}
	switch {
	case loaded.Vector.Enabled && loaded.Vector.People.Enabled:
		lane.Action = planActionKeep
		lane.Reason = "already enabled"
	case !plan.laneOn(laneTextSearch):
		lane.Action = planActionSkip
		lane.Reason = "requires the text-search lane"
	case textLaneProvider(plan) == "custom":
		lane.Action = planActionSkip
		lane.Reason = "custom hosted provider: configure [vector.people] and its postures explicitly, then run `msgvault person provider consent --semantic-embeddings --yes`"
	default:
		lane.Action = planActionEnable
		lane.Gate = textLaneGate(plan)
		lane.Reason = "one curated, non-sensitive attribute document per person rides the text-search generation"
		lane.edits = []config.TableEdit{{
			Path: []string{tomlTableVector, "people"},
			Values: map[string]any{
				"enabled": true, "retention_posture": options.personRetentionPosture, "training_posture": options.personTrainingPosture,
			},
		}}
		lane.next = []string{"msgvault person provider consent --semantic-embeddings --yes"}
	}
	return lane
}

func planVisualSearch(loaded *config.Config, detection setupDetection) setupLanePlan {
	lane := setupLanePlan{Lane: laneVisualSearch, Label: "Visual attachment search", Provider: "voyage", Model: loaded.Vector.Multimodal.Model}
	switch {
	case loaded.Vector.Multimodal.Enabled:
		lane.Action = planActionKeep
		lane.Reason = "already enabled"
	case !detection.voyageKey:
		lane.Action = planActionSkip
		lane.Provider, lane.Model = "", ""
		lane.Reason = "needs " + setupVoyageKeyEnv
	case detection.backendUnavailable != "":
		lane.Action, lane.Reason = planActionPending, detection.backendUnavailable
	case detection.voyageManifest != "":
		lane.Action, lane.Gate = planActionEnable, gateVoyage
		lane.Reason = "probe manifest found at " + detection.voyageManifest
		lane.edits = append([]config.TableEdit{
			{Path: []string{tomlTableVector, "multimodal"}, Values: map[string]any{"enabled": true, "capabilities_file": detection.voyageManifest}},
		}, setupScheduleEdits(detection.configKeys, "multimodal")...)
		lane.next = []string{"msgvault multimodal build --yes"}
	default:
		lane.Action = planActionPending
		lane.Reason = "the provider probe needs private synthetic WebP and MP4 seeds; the lane stays off until the manifest exists"
		lane.edits = setupScheduleEdits(detection.configKeys, "multimodal")
		lane.next = []string{visualProbeCommand(loaded), "msgvault setup providers"}
	}
	return lane
}

func planDocuments(loaded *config.Config, detection setupDetection, options setupProvidersOptions) setupLanePlan {
	documents := loaded.Attachments.Documents
	lane := setupLanePlan{Lane: laneDocuments, Label: "Document attachments", Provider: documents.Provider, Model: documents.Model}
	manifest := setupMistralManifestPath(loaded)
	switch {
	case documents.Enabled:
		lane.Action = planActionKeep
		lane.Reason = "already enabled"
	case !detection.mistralKey:
		lane.Action = planActionSkip
		lane.Provider, lane.Model = "", ""
		lane.Reason = "needs " + detection.mistralKeyEnv
	default:
		lane.Action, lane.Gate = planActionEnable, gateMistral
		lane.Reason = fmt.Sprintf("EU endpoint, %s; recorded postures retention=%s, training=%s (override with --document-retention/--document-training)",
			documents.Model, options.documentRetention, options.documentTraining)
		lane.edits = []config.TableEdit{{
			Path: []string{"attachments", "documents"},
			Values: map[string]any{
				"enabled": true, "retention_posture": options.documentRetention, "training_posture": options.documentTraining,
			},
		}}
		if detection.mistralManifest != "" {
			lane.next = []string{
				"msgvault documents consent-mistral --capabilities " + manifest + " --yes",
				"msgvault documents build --capabilities " + manifest + " --yes",
			}
		} else {
			lane.next = []string{
				"msgvault documents probe-mistral --fixtures <private-fixture-dir> > " + manifest,
				"msgvault documents consent-mistral --capabilities " + manifest + " --yes",
				"msgvault documents build --capabilities " + manifest + " --yes",
			}
		}
	}
	return lane
}

func planDocumentVectors(loaded *config.Config, plan *setupProvidersPlan) setupLanePlan {
	lane := setupLanePlan{Lane: laneDocumentVectors, Label: "Document semantic search"}
	documents := loaded.Attachments.Documents
	switch {
	case documents.Enabled && documents.Index.Embeddings.Enabled:
		lane.Action = planActionKeep
		lane.Reason = "already enabled"
	case !plan.laneOn(laneDocuments):
		lane.Action = planActionSkip
		lane.Reason = "requires the document lane"
	case !plan.laneOn(laneTextSearch):
		lane.Action = planActionSkip
		lane.Reason = "requires the text-search lane"
	case textLaneProvider(plan) == "custom":
		lane.Action = planActionSkip
		lane.Reason = "custom hosted provider: configure [attachments.documents.index.embeddings] explicitly and consent separately to document and query text"
	default:
		lane.Action = planActionEnable
		lane.Gate = textLaneGate(plan)
		lane.Reason = "document chunks and search query text are sent to the text-search provider after separate document and query consents"
		lane.edits = []config.TableEdit{{
			Path: []string{"attachments", "documents", "index", "embeddings"}, Values: map[string]any{"enabled": true},
		}}
		lane.next = []string{
			"msgvault documents vectors consent --yes",
			"msgvault documents vectors consent --purpose queries --yes",
			"msgvault documents vectors build",
		}
	}
	return lane
}

func planPeopleInference(
	loaded *config.Config,
	detection setupDetection,
	plan *setupProvidersPlan,
	options setupProvidersOptions,
	now time.Time,
) (setupLanePlan, *setupInferencePlan) {
	lane := setupLanePlan{Lane: lanePeopleInference, Label: "People sweep"}
	if loaded.People.Sweep.Enabled {
		lane.Action = planActionKeep
		if name, provider, err := loaded.People.Sweep.ActiveProviderConfig(); err == nil {
			lane.Provider, lane.Model = name, provider.Model
		}
		lane.Reason = "already enabled"
		return lane, nil
	}
	if !options.allowSensitive && (detection.openAIKey ||
		(detection.ollama.Reachable && detection.ollamaLoopback && detection.ollama.hasModel(loaded.Chat.Model))) {
		lane.Action = planActionPending
		lane.Reason = "people sweep requires --allow-sensitive: sensitive archive excerpts may be sent to the selected inference provider and used to infer sensitive personal attributes"
		lane.next = []string{"msgvault setup providers --allow-sensitive"}
		return lane, nil
	}
	sources := []string{string(peoplesweep.SourceConversationText), string(peoplesweep.SourceMeetingText)}
	if plan.laneOn(laneDocuments) {
		sources = append(sources, string(peoplesweep.SourceDocumentText))
	}
	since := time.Date(now.Year()-1, time.January, 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	base := personProviderAddOptions{
		custom: true, protocol: string(peoplesweep.ProtocolOpenAIChat),
		retentionPosture: options.retentionPosture, trainingPosture: options.trainingPosture,
		allowedSources: sources, sourceSince: since, allowSensitive: options.allowSensitive,
		requestTimeout: time.Minute, confirmed: true,
	}
	switch {
	case detection.openAIKey:
		if _, exists := loaded.People.Sweep.Providers[setupInferenceProfile]; exists {
			lane.Action = planActionSkip
			lane.Provider = setupInferenceProfile
			lane.Reason = "profile exists but the sweep is off; run `msgvault person provider consent " +
				setupInferenceProfile + " --yes` and `msgvault person provider use " + setupInferenceProfile + "`"
			return lane, nil
		}
		base.endpoint, base.model, base.auth = setupOpenAIEndpoint, setupInferenceModel, string(peoplesweep.AuthBearer)
		base.credentialEnv, base.reasoningEffort = setupOpenAIKeyEnv, setupInferenceReasoning
		lane.Action, lane.Provider, lane.Model, lane.Gate = planActionOnboard, setupInferenceProfile, setupInferenceModel, gateOpenAI
		lane.Reason = fmt.Sprintf("openai_chat profile %q at %s reasoning; sensitive archive excerpts from %s since %s may be sent to OpenAI and used to infer sensitive personal attributes; extraction runs for tracked people only",
			setupInferenceProfile, setupInferenceReasoning, strings.Join(sources, ", "), since)
		lane.next = []string{"msgvault person track <person-id>"}
		return lane, &setupInferencePlan{name: setupInferenceProfile, options: base, gate: gateOpenAI}
	case detection.ollama.Reachable && detection.ollamaLoopback && detection.ollama.hasModel(loaded.Chat.Model):
		if _, exists := loaded.People.Sweep.Providers[setupOllamaProfile]; exists {
			lane.Action = planActionSkip
			lane.Provider = setupOllamaProfile
			lane.Reason = "profile exists but the sweep is off; run `msgvault person provider consent " +
				setupOllamaProfile + " --yes` and `msgvault person provider use " + setupOllamaProfile + "`"
			return lane, nil
		}
		// Only the configured chat model is eligible: picking whatever the
		// server lists could select an embedding-only model whose check fails
		// after the profile is already published.
		model := loaded.Chat.Model
		base.endpoint, base.model, base.auth = detection.ollamaEndpoint, model, string(peoplesweep.AuthNone)
		lane.Action, lane.Provider, lane.Model = planActionOnboard, setupOllamaProfile, model
		lane.Reason = "local Ollama server at " + detection.ollamaEndpoint + "; sensitive archive excerpts may be used to infer sensitive personal attributes; evidence stays on this machine"
		lane.next = []string{"msgvault person track <person-id>"}
		return lane, &setupInferencePlan{name: setupOllamaProfile, options: base}
	case detection.ollama.Reachable && !detection.ollamaLoopback:
		lane.Action = planActionSkip
		lane.Reason = "Ollama at " + detection.ollamaEndpoint + " is not loopback; add an authenticated profile with `msgvault person provider add`"
	case detection.ollama.Reachable:
		lane.Action = planActionSkip
		lane.Reason = "Ollama has no " + loaded.Chat.Model + "; run `ollama pull " + loaded.Chat.Model +
			"` or set [chat].model to an available chat model, then re-run setup"
	default:
		lane.Action = planActionSkip
		lane.Reason = "needs " + setupOpenAIKeyEnv + " or a local Ollama server"
	}
	return lane, nil
}

// gateDisclosure states plainly what each hosted provider receives once the
// operator answers yes.
func gateDisclosure(gate string, plan *setupProvidersPlan) string {
	var lines []string
	switch gate {
	case gateVoyage:
		lines = append(lines, "Voyage AI ("+setupVoyageEndpoint+") receives:")
		if plan.laneOn(laneTextSearch) && textLaneProvider(plan) == "voyage" {
			lines = append(lines, "  - message, chat, and meeting text for embeddings ("+setupVoyageTextModel+")")
		}
		if plan.laneOn(lanePersonSearch) && textLaneProvider(plan) == "voyage" {
			lines = append(lines, "  - one curated, non-sensitive attribute document per person")
		}
		if plan.laneOn(laneVisualSearch) {
			lines = append(lines, "  - eligible image and video attachment bytes with bounded message context, after `msgvault multimodal build --yes`")
		}
		if plan.laneOn(laneDocumentVectors) && textLaneProvider(plan) == "voyage" {
			lines = append(lines, "  - extracted document text, after `msgvault documents vectors consent --yes`",
				"  - document search query text, after `msgvault documents vectors consent --purpose queries --yes`")
		}
	case gateMistral:
		lines = append(lines,
			"Mistral (EU region, "+documentindex.ModelMistralOCR+") receives:",
			"  - complete original bytes of standalone document attachments, only after the probe manifest and `msgvault documents consent-mistral --yes`",
			"  - postures recorded now: retention="+planValue(plan, laneDocuments, "retention_posture")+", training="+planValue(plan, laneDocuments, "training_posture"))
	case gateOpenAI:
		lines = append(lines, "OpenAI ("+setupOpenAIEndpoint+") receives:")
		if plan.laneOn(laneTextSearch) && textLaneProvider(plan) == "openai" {
			lines = append(lines, "  - message, chat, and meeting text for embeddings ("+setupOpenAITextModel+")")
		}
		if plan.laneOn(lanePersonSearch) && textLaneProvider(plan) == "openai" {
			lines = append(lines, "  - one curated, non-sensitive attribute document per person")
		}
		if plan.laneOn(laneDocumentVectors) && textLaneProvider(plan) == "openai" {
			lines = append(lines, "  - extracted document text, after `msgvault documents vectors consent --yes`",
				"  - document search query text, after `msgvault documents vectors consent --purpose queries --yes`")
		}
		if plan.inference != nil && plan.inference.gate == gateOpenAI {
			lines = append(lines, "  - bounded evidence packets of "+
				strings.Join(plan.inference.options.allowedSources, ", ")+
				" for tracked people ("+setupInferenceModel+"); a synthetic check request is sent now",
				"  - --allow-sensitive authorizes sending sensitive archive excerpts to OpenAI and inferring sensitive personal attributes")
		}
	}
	return strings.Join(lines, "\n")
}

func textLaneProvider(plan *setupProvidersPlan) string {
	for _, item := range plan.Lanes {
		if item.Lane == laneTextSearch {
			return item.Provider
		}
	}
	return ""
}

func planValue(plan *setupProvidersPlan, lane, key string) string {
	for _, item := range plan.Lanes {
		if item.Lane != lane {
			continue
		}
		for _, edit := range item.edits {
			if value, ok := edit.Values[key]; ok {
				return fmt.Sprint(value)
			}
		}
	}
	return ""
}

func writeSetupPlan(w io.Writer, configPath string, plan *setupProvidersPlan) {
	_, _ = fmt.Fprintf(w, "Provider setup plan for %s\n", configPath)
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, lane := range plan.Lanes {
		_, _ = fmt.Fprintf(table, "  %s\t%s\t%s\t%s\t%s\n", lane.Label, lane.Action, dash(lane.Provider), dash(lane.Model), lane.Reason)
	}
	_ = table.Flush()
}

type setupProvidersOutput struct {
	Plan      []setupLanePlan `json:"plan"`
	Applied   bool            `json:"applied"`
	DryRun    bool            `json:"dry_run"`
	Declined  []string        `json:"declined,omitempty"`
	FollowUps []string        `json:"follow_ups,omitempty"`
	Report    laneReport      `json:"report"`
}

func newSetupProvidersCommand(deps setupProvidersDeps) *cobra.Command {
	var options setupProvidersOptions
	command := &cobra.Command{
		Use:   "providers",
		Short: "Turn on the retrieval and people lanes the available API keys support, with recommended defaults",
		Long: `Read the environment and configure every lane that is still unset:

  ` + setupVoyageKeyEnv + `   text search with Voyage contextual embeddings (conversation
                   windows for chats, turn-aware chunks for meetings), semantic
                   people search, and the visual attachment lane once its probe
                   manifest exists
  ` + setupOpenAIKeyEnv + `   text search on the OpenAI-compatible path when no Voyage key
                   is present, and the people sweep on ` + setupInferenceModel + `
  MISTRAL_API_KEY  document attachment extraction, plus document vectors when a
                   text lane is on
  (no keys)        a local Ollama server at [chat].server when it is reachable

Hosted lanes never turn on from a key alone: setup asks once per provider,
writes the recommended values to config.toml, runs the people-provider
check and consent, and prints what is on, what is off, and why. Lanes that
are already configured are left alone, so re-running after adding a key
upgrades only that lane. Probe manifests are expected at
<home>/` + setupVoyageManifestName + ` and <home>/` + setupMistralManifestName + `.

The people sweep also requires --allow-sensitive: archive excerpts may contain
sensitive details and may be used to infer sensitive personal attributes.
--yes accepts provider prompts but does not grant this separate opt-in.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runSetupProviders(command, deps, options)
		},
	}
	flags := command.Flags()
	flags.BoolVar(&options.yes, "yes", false, "Accept every provider disclosure without prompting")
	flags.BoolVar(&options.dryRun, "dry-run", false, "Print the plan and the current lane report without writing anything")
	flags.BoolVar(&options.jsonOutput, flagJSON, false, "Output structured JSON")
	flags.BoolVar(&options.allowSensitive, "allow-sensitive", false,
		"Allow the people sweep to send sensitive archive excerpts to its inference provider and infer sensitive personal attributes")
	flags.StringVar(&options.documentRetention, "document-retention", documentindex.RetentionStandard,
		"Mistral retention posture to record: standard or zdr")
	flags.StringVar(&options.documentTraining, "document-training", documentindex.TrainingDefaultOptOut,
		"Mistral training posture to record: default-opt-out or opted-out")
	flags.StringVar(&options.retentionPosture, "retention-posture", setupPostureDeclared,
		"Retention assertion recorded for embedding and inference providers")
	flags.StringVar(&options.trainingPosture, "training-posture", setupPostureDeclared,
		"Training assertion recorded for embedding and inference providers")
	return command
}

func runSetupProviders(command *cobra.Command, deps setupProvidersDeps, options setupProvidersOptions) error {
	if err := validateSetupProvidersOptions(options); err != nil {
		return err
	}
	if deps.remoteConfigured != nil && deps.remoteConfigured() {
		return errors.New("setup providers cannot run against a configured remote daemon: it edits this machine's config.toml, which the remote daemon never reads; run it on the daemon host, or pass --local to configure a daemon on this machine")
	}
	if deps.readConfigFile == nil || deps.editConfigTables == nil || deps.loadConfig == nil {
		return errors.New("setup providers config editing is unavailable")
	}
	ctx := command.Context()
	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	loaded, err := deps.loadConfig(before)
	if err != nil {
		return err
	}
	// Read saved assertions before config defaults turn an absent document
	// posture into "unknown". Only the corresponding explicit flag replaces
	// an existing assertion; inference still uses its own command defaults.
	var saved config.Config
	keys, err := toml.Decode(string(before.Content), &saved)
	if err != nil {
		return fmt.Errorf("read saved provider settings: %w", err)
	}
	options.personRetentionPosture = setupPosture(saved.Vector.People.RetentionPosture, options.retentionPosture, command.Flags().Changed("retention-posture"))
	options.personTrainingPosture = setupPosture(saved.Vector.People.TrainingPosture, options.trainingPosture, command.Flags().Changed("training-posture"))
	options.documentRetention = setupPosture(saved.Attachments.Documents.RetentionPosture, options.documentRetention, command.Flags().Changed("document-retention"))
	options.documentTraining = setupPosture(saved.Attachments.Documents.TrainingPosture, options.documentTraining, command.Flags().Changed("document-training"))
	now := time.Now()
	if deps.now != nil {
		now = deps.now()
	}
	detection := detectSetupProviders(ctx, loaded, deps)
	detection.configKeys = keys
	plan := planSetupProviders(loaded, detection, options, now)
	out := command.OutOrStdout()
	if !options.jsonOutput {
		writeSetupPlan(out, loaded.ConfigFilePath(), &plan)
		_, _ = fmt.Fprintln(out)
	}

	var declined []string
	gates := plan.gates()
	if options.dryRun {
		if !options.jsonOutput {
			for _, gate := range gates {
				_, _ = fmt.Fprintln(out, gateDisclosure(gate, &plan))
			}
			_, _ = fmt.Fprintln(out, "Dry run: nothing written.")
			_, _ = fmt.Fprintln(out)
		}
		return writeSetupProvidersResult(command, deps, loaded, &plan, options, false, declined)
	}
	if len(gates) > 0 && !options.yes {
		if deps.isTerminal == nil || !deps.isTerminal(command) || options.jsonOutput {
			return fmt.Errorf("setup providers needs one consent per hosted provider (%s); review the plan with --dry-run, then re-run with --yes",
				strings.Join(gates, ", "))
		}
		reader := bufio.NewReader(command.InOrStdin())
		for _, gate := range gates {
			if !slices.Contains(plan.gates(), gate) {
				continue
			}
			_, _ = fmt.Fprintln(out, gateDisclosure(gate, &plan))
			_, _ = fmt.Fprintf(out, "Enable the %s lanes? [y/N]: ", gate)
			answer, readErr := reader.ReadString('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read consent answer: %w", readErr)
			}
			if !isYesAnswer(strings.ToLower(strings.TrimSpace(answer))) {
				declined = append(declined, gate)
				plan.declineGate(gate)
				plan.refreshDependencies(loaded, detection, options, now)
			}
			_, _ = fmt.Fprintln(out)
		}
	} else if !options.jsonOutput {
		for _, gate := range gates {
			_, _ = fmt.Fprintln(out, gateDisclosure(gate, &plan))
			_, _ = fmt.Fprintf(out, "Accepted with --yes.\n\n")
		}
	}

	if err := applySetupProvidersPlan(command, deps, before, &plan, options.jsonOutput); err != nil {
		return err
	}
	return writeSetupProvidersResult(command, deps, nil, &plan, options, plan.writes(), declined)
}

func setupPosture(saved, proposed string, override bool) string {
	if saved != "" && !override {
		return saved
	}
	return proposed
}

// The people-provider commands need a published profile for daemon-owned
// checks and consent. Keep their existing gates and restore the entire setup
// configuration if any step fails. Retain each publication's identity so a
// concurrent edit is never adopted as our rollback target.
func applySetupProvidersPlan(command *cobra.Command, deps setupProvidersDeps, before config.ConfigFile, plan *setupProvidersPlan, quiet bool) (retErr error) {
	edits := plan.mergedEdits()
	if len(edits) == 0 && plan.inference == nil {
		return nil
	}
	if deps.restoreConfigFile == nil {
		return errors.New("setup providers config rollback is unavailable")
	}
	current := before
	changed := false
	defer func() {
		if retErr != nil && changed && current.Exists {
			if _, err := deps.restoreConfigFile(current, before); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restore setup config: %w", err))
			}
		}
	}()
	record := func(snapshot config.ConfigFile, err error) (config.ConfigFile, error) {
		if err == nil || (errors.Is(err, config.ErrConfigChanged) && snapshot.Exists) {
			current = snapshot
			changed = true
		}
		return snapshot, err
	}
	edit := func(etag string, edits []config.TableEdit) (config.ConfigFile, error) {
		if etag != current.ETag {
			return config.ConfigFile{}, config.ErrConfigConflict
		}
		return record(deps.editConfigTables(etag, edits))
	}
	if len(edits) > 0 {
		if err := config.ValidateConfigTableEdits(before, edits); err != nil {
			return fmt.Errorf("planned config changes are invalid: %w", err)
		}
		if _, err := edit(before.ETag, edits); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
	}
	if plan.inference != nil {
		if deps.personProvider == nil {
			return errors.New("people provider onboarding is unavailable")
		}
		provider := deps.personProvider()
		provider.readConfigFile = func() (config.ConfigFile, error) {
			snapshot, err := deps.readConfigFile()
			if err == nil && (snapshot.Exists != current.Exists ||
				(current.Exists && !config.SameConfigFileVersion(snapshot, current))) {
				return config.ConfigFile{}, config.ErrConfigConflict
			}
			return snapshot, err
		}
		provider.editConfigTables = edit
		provider.restoreConfigFile = func(published, previous config.ConfigFile) (config.ConfigFile, error) {
			return record(deps.restoreConfigFile(published, previous))
		}
		if err := onboardSetupInferenceProfile(command, provider, *plan.inference, quiet); err != nil {
			return err
		}
	}
	return nil
}

// writeSetupProvidersResult reloads the config (unless the caller passes the
// unchanged one) and prints the lane report plus follow-up commands.
func writeSetupProvidersResult(
	command *cobra.Command,
	deps setupProvidersDeps,
	loaded *config.Config,
	plan *setupProvidersPlan,
	options setupProvidersOptions,
	applied bool,
	declined []string,
) error {
	ctx := command.Context()
	if loaded == nil {
		snapshot, err := deps.readConfigFile()
		if err != nil {
			return err
		}
		loaded, err = deps.loadConfig(snapshot)
		if err != nil {
			return err
		}
	}
	env := setupEnvironment{lookupEnv: deps.lookupEnv, fileExists: deps.fileExists}
	if deps.consentState != nil {
		env.consent = deps.consentState(ctx, loaded)
	}
	report := buildLaneReport(loaded, env)
	followUps := setupFollowUps(ctx, deps, loaded, plan, applied)
	if options.jsonOutput {
		encoder := json.NewEncoder(command.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(setupProvidersOutput{
			Plan: plan.Lanes, Applied: applied, DryRun: options.dryRun,
			Declined: declined, FollowUps: followUps, Report: report,
		})
	}
	out := command.OutOrStdout()
	if applied {
		_, _ = fmt.Fprintf(out, "Configuration written to %s\n\n", loaded.ConfigFilePath())
	}
	if err := writeLaneReport(out, report, false); err != nil {
		return err
	}
	if len(followUps) > 0 {
		_, _ = fmt.Fprintln(out)
		if options.dryRun {
			_, _ = fmt.Fprintln(out, "Next steps after applying:")
		} else {
			_, _ = fmt.Fprintln(out, "Next steps:")
		}
		for i, step := range followUps {
			_, _ = fmt.Fprintf(out, "  %d. %s\n", i+1, step)
		}
	}
	return nil
}

// setupFollowUps orders the commands that finish what setup started: restart
// a running daemon first so it loads the new lanes, then consents and builds.
func setupFollowUps(ctx context.Context, deps setupProvidersDeps, loaded *config.Config, plan *setupProvidersPlan, applied bool) []string {
	var steps []string
	seen := map[string]bool{}
	add := func(step string) {
		if step != "" && !seen[step] {
			seen[step] = true
			steps = append(steps, step)
		}
	}
	if applied && deps.daemonAlive != nil && deps.daemonAlive(ctx, loaded) {
		add("msgvault daemon restart")
	}
	for _, lane := range plan.Lanes {
		if lane.Action == planActionEnable || lane.Action == planActionPending || lane.Action == planActionOnboard {
			for _, step := range lane.next {
				add(step)
			}
		}
	}
	sort.SliceStable(steps, func(i, j int) bool {
		// Probes and consents before builds, so the printed order is runnable.
		return followUpRank(steps[i]) < followUpRank(steps[j])
	})
	return steps
}

func followUpRank(step string) int {
	switch {
	case step == "msgvault daemon restart":
		return 0
	case strings.Contains(step, " probe"):
		return 1
	case strings.Contains(step, " consent"):
		return 2
	case strings.Contains(step, "setup providers"):
		return 3
	case strings.Contains(step, " build"):
		return 4
	default:
		return 5
	}
}

// onboardSetupInferenceProfile publishes, checks, consents to, and selects
// one people-sweep profile through the same path `person provider add`,
// `consent`, and `use` take, so setup never bypasses a gate those commands
// enforce. Existing profiles are not re-added; a missing consent or
// selection is completed.
func onboardSetupInferenceProfile(
	command *cobra.Command,
	deps personProviderCommandDeps,
	plan setupInferencePlan,
	quiet bool,
) error {
	if deps.readConfigFile == nil {
		return errors.New("people provider config editing is unavailable")
	}
	// The add step's own summary tells the operator to run consent and use
	// next, which setup does itself, so that step always runs silently; the
	// consent disclosure and the selection notice are kept unless --json.
	silent := silentSetupCommand(command)
	target := command
	if quiet {
		target = silent
	}
	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	sweep, err := personProviderConfigFromSnapshot(deps, before)
	if err != nil {
		return err
	}
	if _, exists := sweep.Providers[plan.name]; !exists {
		if err := runPersonProviderAdd(silent, deps, plan.name, plan.options); err != nil {
			return fmt.Errorf("onboard people provider %q: %w", plan.name, err)
		}
		if !quiet {
			_, _ = fmt.Fprintf(command.OutOrStdout(), "Added and checked people provider profile %q.\n", plan.name)
		}
	}
	after, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	sweep, err = personProviderConfigFromSnapshot(deps, after)
	if err != nil {
		return err
	}
	selected, err := selectPersonProviderConfig(sweep, plan.name)
	if err != nil {
		return err
	}
	selected.Enabled = true
	if err := consentSetupInferenceProfile(target, deps, plan.name, selected); err != nil {
		return fmt.Errorf("consent to people provider %q: %w", plan.name, err)
	}
	if sweep.Enabled && sweep.Provider.Name == plan.name {
		return nil
	}
	useDeps := deps
	useDeps.config = func() peoplesweep.Config { return sweep }
	if err := runPersonProviderUse(target, useDeps, plan.name, false); err != nil {
		return fmt.Errorf("select people provider %q: %w", plan.name, err)
	}
	return nil
}

// silentSetupCommand mirrors command's context and stdin with discarded
// standard output, for steps whose own summary would contradict the pass.
func silentSetupCommand(command *cobra.Command) *cobra.Command {
	silent := &cobra.Command{Use: command.Use}
	silent.SetContext(command.Context())
	silent.SetOut(io.Discard)
	silent.SetErr(command.ErrOrStderr())
	silent.SetIn(command.InOrStdin())
	return silent
}

// consentSetupInferenceProfile records consent directly when this process
// may write the archive, and otherwise proxies `person provider consent
// <name> --yes` to the daemon that owns it.
func consentSetupInferenceProfile(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	selected peoplesweep.Config,
) error {
	directStore, _, err := personProviderMutationScope(command.Context(), deps)
	if err != nil {
		return err
	}
	if directStore {
		consentDeps := deps
		consentDeps.config = func() peoplesweep.Config { return selected }
		return runPersonProviderConsent(command, consentDeps, true, false, false)
	}
	if deps.proxy == nil {
		return errors.New("people provider daemon proxy is unavailable")
	}
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	provider := &cobra.Command{Use: personProviderCommandName}
	leaf := &cobra.Command{Use: cmdUseConsent}
	leaf.Flags().Bool("yes", false, "")
	if err := leaf.Flags().Set("yes", "true"); err != nil {
		return fmt.Errorf("set people provider consent flag: %w", err)
	}
	provider.AddCommand(leaf)
	person.AddCommand(provider)
	root.AddCommand(person)
	leaf.SetOut(command.OutOrStdout())
	leaf.SetErr(command.ErrOrStderr())
	return deps.proxy(leaf, []string{name}, nil)
}
