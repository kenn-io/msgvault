package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type localEmbeddingsController struct {
	gate       api.OperationGate
	state      *invocation
	store      *store.Store
	dbPath     string
	handle     *vectorInitHandle
	server     *api.Server
	scheduler  *scheduler.Scheduler
	runner     api.CLIRunner
	refreshMCP func(context.Context, *invocation) error
	openers    []visual.StreamOpener
	inspect    func(context.Context, embedlocal.Options) (embedlocal.Status, error)
	install    func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error)
}

func (c *localEmbeddingsController) Inspect(ctx context.Context) (api.LocalEmbeddingsStatus, error) {
	dir, err := localEmbeddingsDir()
	if err != nil {
		return api.LocalEmbeddingsStatus{}, err
	}
	status, _, err := c.inspectAvailable(ctx, dir, c.state.cfg)
	if err != nil {
		return api.LocalEmbeddingsStatus{}, err
	}
	c.handle.mu.Lock()
	cfg := c.state.cfg.Vector
	if c.handle.vf != nil {
		cfg = c.handle.vf.Cfg
	}
	c.handle.mu.Unlock()
	provider := ""
	if cfg.Enabled || cfg.Embeddings.LocalOnly || cfg.Embeddings.Endpoint != "" || cfg.Embeddings.Model != "" {
		switch {
		case cfg.Embeddings.LocalOnly:
			provider = "builtin"
		case isOllamaEmbeddingEndpoint(cfg.Embeddings.Endpoint, c.state.cfg.Chat.Server):
			provider = "ollama"
		default:
			provider = "configured"
		}
	}
	return api.LocalEmbeddingsStatus{DocumentRebuild: provider == "ollama" && c.state.cfg.Attachments.Documents.Index.Embeddings.Enabled, Enabled: cfg.Enabled, Provider: provider, Compatible: cfg.Embeddings.LocalCompatible(),
		OllamaAvailable: status.OllamaAvailable, DownloadBytes: status.DownloadBytes, Phase: "idle"}, nil
}

// Inspect the configured local Ollama server as CLI setup does. Kit covers its default port.
func (c *localEmbeddingsController) inspectAvailable(ctx context.Context, dir string, cfg *config.Config) (embedlocal.Status, string, error) {
	status, err := c.inspect(ctx, embedlocal.Options{DataDir: dir})
	server := strings.TrimRight(strings.TrimSpace(cfg.Chat.Server), "/")
	var endpoint string
	if err == nil && !cfg.Vector.Embeddings.LocalOnly && cfg.Vector.Embeddings.Endpoint == "" && embeddingProviderName(server) == "local" && server != "http://localhost:11434" && server != "http://127.0.0.1:11434" {
		endpoint = ollamaTextEndpoint(server, probeOllamaServer(ctx, server))
		status.OllamaAvailable = status.OllamaAvailable || endpoint != ""
	}
	return status, endpoint, err
}

func isOllamaEmbeddingEndpoint(endpoint, configuredServer string) bool {
	server := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(endpoint), "/"), "/v1")
	return server == "http://127.0.0.1:11434" || server == "http://localhost:11434" || (configuredServer != "" && server == strings.TrimRight(configuredServer, "/"))
}

func (c *localEmbeddingsController) Enable(ctx context.Context, builtin bool, report func(api.LocalEmbeddingsStatus)) error {
	// Large archives may take hours; retries resume an unfinished message build.
	runtimeCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	if !c.handle.WaitContext(ctx) {
		return ctx.Err()
	}

	snapshot, err := config.ReadConfigFile(c.state.cfg.ConfigFilePath())
	if err != nil {
		return err
	}
	loaded, err := c.state.cfg.ReloadConfigFile(snapshot)
	if err != nil {
		return err
	}
	// A configured lane changes provider only through Use built-in instead.
	if !loaded.Vector.Embeddings.LocalOnly && !isOllamaEmbeddingEndpoint(loaded.Vector.Embeddings.Endpoint, loaded.Chat.Server) && (loaded.Vector.Enabled || loaded.Vector.Embeddings.Endpoint != "" || loaded.Vector.Embeddings.Model != "") {
		return errors.New("search by meaning already uses a configured provider; change its settings before selecting local search")
	}
	dir, err := localEmbeddingsDir()
	if err != nil {
		return err
	}
	available, ollamaEndpoint, err := c.inspectAvailable(ctx, dir, loaded)
	if err != nil {
		return err
	}
	var configKeys toml.MetaData
	configKeys, err = toml.Decode(string(snapshot.Content), &map[string]any{})
	if err != nil {
		return fmt.Errorf("read local search settings: %w", err)
	}
	backend, unavailable := setupVectorBackend(c.state.cfg)
	configured := loaded.Vector.Embeddings.LocalOnly || loaded.Vector.Embeddings.Endpoint != "" || loaded.Vector.Embeddings.Model != ""
	useOllama := !builtin && !loaded.Vector.Embeddings.LocalOnly && (configured || available.OllamaAvailable)
	detection := setupDetection{backend: backend, backendUnavailable: unavailable, configKeys: configKeys, localEmbeddings: !useOllama, resumeLocalText: configured && !builtin}
	if useOllama && !configured {
		detection.embeddingEndpoint = ollamaEndpoint
		if detection.embeddingEndpoint == "" {
			detection.embeddingEndpoint, err = localEmbeddingEndpoint(ctx, embedlocal.Options{DataDir: dir})
			if err != nil {
				return err
			}
		}
		detection.ollamaEndpoint = detection.embeddingEndpoint
	}
	lane := planTextSearch(loaded, detection)
	if lane.Action != planActionEnable {
		return errors.New(lane.Reason)
	}
	nextConfig, err := config.PreviewConfigTableEdits(snapshot, lane.edits)
	if err != nil {
		return err
	}
	var builtinEndpoint string
	if !useOllama {
		installation, err := c.install(ctx, embedlocal.InstallOptions{DataDir: dir,
			Confirm: func(context.Context, int64) (bool, error) { return true, nil },
			Progress: func(done, total int64) {
				if done == 0 {
					return
				}
				report(api.LocalEmbeddingsStatus{Phase: "downloading", DoneBytes: done, TotalBytes: total})
			},
		})
		if err != nil {
			return err
		}
		builtinEndpoint, err = localEmbeddingEndpoint(ctx, embedlocal.Options{DataDir: installation.DataDir, Executable: installation.Executable, SkipOllama: true})
		if err != nil {
			return err
		}
	}
	if c.gate != nil {
		begin := c.gate.BeginWorkContext
		if labeled, ok := c.gate.(api.LabeledOperationGate); ok {
			begin = func(ctx context.Context) (func(), bool) {
				return labeled.BeginLabeledWorkContext(ctx, "msgvault embeddings build")
			}
		}
		release, ok := begin(ctx)
		if !ok {
			return errors.New("the daemon is stopping")
		}
		defer release()
	}
	// Apply only this lane to the running daemon; other saved changes remain pending.
	runtimeConfig := *c.state.cfg
	c.handle.mu.Lock()
	if c.handle.vf != nil {
		runtimeConfig.Vector = c.handle.vf.Cfg
	}
	c.handle.mu.Unlock()
	previousConfig := runtimeConfig
	previousState := *c.state
	previousState.cfg = &previousConfig
	runtimeConfig.Vector.Enabled = true
	runtimeConfig.Vector.Backend = nextConfig.Vector.Backend
	runtimeConfig.Vector.Embeddings = nextConfig.Vector.Embeddings
	runtimeConfig.Vector.Embed.Schedule = nextConfig.Vector.Embed.Schedule
	state := *c.state
	state.cfg = &runtimeConfig
	nextCtx := withInvocation(runtimeCtx, &state)
	workCtx := withInvocation(ctx, &state)
	vecCfg, err := resolvedVectorConfig(workCtx, c.store, runtimeConfig.Vector)
	if err != nil {
		return err
	}
	if !useOllama {
		vecCfg.Embeddings.Endpoint = builtinEndpoint
	}
	if _, err := config.EditConfigTables(loaded.ConfigFilePath(), snapshot.ETag, lane.edits); err != nil {
		return err
	}
	rebuildDocuments, err := c.retainLocalSearchConsents(workCtx, &previousState, &state)
	if err != nil {
		return err
	}
	var vf *vectorFeatures
	if c.handle.vf == nil {
		vf, err = setupVectorFeaturesForRun(nextCtx, c.store, c.dbPath, false, c.openers...)
	} else {
		vf, err = c.handle.vf.configureText(nextCtx, &state, vecCfg)
	}
	if err != nil {
		return err
	}
	if vf == nil {
		return errors.New("local search is unavailable in this build")
	}
	c.handle.mu.Lock()
	c.handle.vf = vf
	c.handle.mu.Unlock()
	publishTextVectorFeatures(nextCtx, c.store, c.server, c.scheduler, vf, &state)
	if rebuildDocuments {
		if _, err := c.scheduler.StartJob("scheduled document indexing", false); err != nil {
			return err
		}
	}
	if c.refreshMCP != nil {
		if err := c.refreshMCP(nextCtx, &state); err != nil {
			return err
		}
	}
	report(api.LocalEmbeddingsStatus{Phase: "indexing"})
	fingerprint := vf.Cfg.GenerationFingerprint()
	building, err := vf.Backend.BuildingGeneration(ctx)
	if err != nil {
		return err
	}
	// Keep the initial build on the published configuration while settings remain editable.
	file, err := os.CreateTemp("", "msgvault-local-search-*.toml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	encodeErr := toml.NewEncoder(file).Encode(&runtimeConfig)
	if err := errors.Join(encodeErr, file.Close()); err != nil {
		return err
	}
	run := func(args ...string) error {
		args = append(args, "--config", file.Name(), "--home", runtimeConfig.HomeDir)
		var stderr string
		err := c.runner.RunCLICommand(workCtx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
			if event.Type == cliStreamStderr {
				stderr += event.Data
				if len(stderr) > 8192 {
					stderr = stderr[len(stderr)-8192:]
				}
			}
			return nil
		})
		if detail := strings.TrimSpace(stderr); err != nil && detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	if building != nil && building.Fingerprint != fingerprint {
		if err := run("embeddings", "retire", strconv.FormatInt(int64(building.ID), 10), "--yes"); err != nil {
			return err
		}
		building = nil
	}
	args := []string{"embeddings", "build", "--yes"}
	if building == nil {
		active, err := vf.Backend.ActiveGeneration(ctx)
		if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
			return fmt.Errorf("read active index: %w", err)
		}
		if errors.Is(err, vector.ErrNoActiveGeneration) || active.Fingerprint != fingerprint {
			args = append(args, "--full-rebuild")
		}
	}
	if err := run(args...); err != nil {
		return err
	}
	return nil
}

// Keep authorized search consumers on the explicitly selected local provider.
func (c *localEmbeddingsController) retainLocalSearchConsents(ctx context.Context, before, after *invocation) (bool, error) {
	if before.cfg.Vector.Enabled && before.cfg.Vector.People.Enabled {
		profile, err := before.cfg.Vector.SemanticPersonEmbeddingProfile()
		if err != nil {
			return false, err
		}
		active, err := c.store.HasActivePersonSemanticEmbeddingConsent(ctx, profile.Fingerprint)
		if err != nil {
			return false, err
		}
		if active {
			next, err := after.cfg.Vector.SemanticPersonEmbeddingProfile()
			if err != nil {
				return false, err
			}
			if _, err := c.store.EnsurePersonSemanticEmbeddingProfile(ctx, next); err != nil {
				return false, err
			}
			if _, _, err := c.store.GrantPersonSemanticEmbeddingConsent(ctx, next.Fingerprint, personProviderConsentActor); err != nil {
				return false, err
			}
		}
	}
	if !before.cfg.Attachments.Documents.Index.Embeddings.Enabled {
		return false, nil
	}
	oldSpec, err := configuredDocumentVectorSpec(withInvocation(ctx, before), c.store)
	if errors.Is(err, store.ErrDocumentVectorInvalidGenerationState) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	newSpec, err := configuredDocumentVectorSpec(ctx, c.store)
	if err != nil {
		return false, err
	}
	rebuild := false
	for _, purpose := range []string{"document_embedding", "query_embedding"} {
		oldConsent, err := configuredDocumentVectorConsentSpec(oldSpec, before, purpose)
		if err != nil {
			return false, err
		}
		consent, err := c.store.GetDocumentVectorConsent(ctx, oldConsent.EgressFingerprint)
		if err != nil {
			return false, err
		}
		if consent == nil || consent.DocumentVectorConsentSpec != oldConsent {
			continue
		}
		next, err := configuredDocumentVectorConsentSpec(newSpec, after, purpose)
		if err != nil {
			return false, err
		}
		if _, _, err := c.store.RecordDocumentVectorConsent(ctx, next, time.Now()); err != nil {
			return false, err
		}
		rebuild = rebuild || purpose == "document_embedding"
	}
	return rebuild, nil
}
