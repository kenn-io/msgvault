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

type localEmbeddingsConflictError struct{ cause error }

func (localEmbeddingsConflictError) Error() string {
	return "Settings changed during setup. Turn on to retry."
}

func (e localEmbeddingsConflictError) Unwrap() error {
	return e.cause
}

type localEmbeddingsInterruptedError struct{ cause error }

func (localEmbeddingsInterruptedError) Error() string {
	return "Message index build interrupted. Use Build index to finish."
}

func (e localEmbeddingsInterruptedError) Unwrap() error {
	return e.cause
}

func (c *localEmbeddingsController) Inspect(ctx context.Context) (api.LocalEmbeddingsStatus, error) {
	if _, unavailable := setupVectorBackend(c.state.cfg); unavailable != "" {
		return api.LocalEmbeddingsStatus{}, errors.New(unavailable)
	}
	host, err := os.Hostname()
	if err != nil {
		return api.LocalEmbeddingsStatus{}, fmt.Errorf("read local search host: %w", err)
	}

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
	missingRuntime := c.handle.vf == nil
	if c.handle.vf != nil {
		cfg = c.handle.vf.Cfg
	}
	c.handle.mu.Unlock()
	provider := ""
	if cfg.Enabled || cfg.Embeddings.LocalOnly || cfg.Embeddings.Endpoint != "" || cfg.Embeddings.Model != "" {
		switch {
		case cfg.Embeddings.LocalOnly:
			provider = "builtin"
		case isOllamaEmbeddingEndpoint(ctx, cfg.Embeddings.Endpoint):
			provider = "ollama"
		default:
			provider = "configured"
		}
	}
	var startupError string
	select {
	case <-c.handle.done:
		if missingRuntime && (provider == "builtin" || provider == "ollama") {
			cfg.Enabled = false
			if state, detail := c.server.VectorStatus(); state == api.VectorStatusError {
				startupError = strings.Replace(detail, "local embeddings need setup: run msgvault setup providers --local-embeddings", "Local model files need setup. Turn on to retry.", 1)
				if startupError == detail {
					startupError = fmt.Sprintf("Search by meaning couldn't start: %s. Turn on to retry.", strings.TrimRight(detail, "."))
				}
			}
		}
	default:
	}
	return api.LocalEmbeddingsStatus{Host: host, DocumentRebuild: provider == "ollama" && c.state.cfg.Attachments.Documents.Index.Embeddings.Enabled, Enabled: cfg.Enabled, Provider: provider, Compatible: cfg.Embeddings.LocalCompatible(),
		OllamaAvailable: status.OllamaAvailable || provider == "ollama", DownloadBytes: status.DownloadBytes, Phase: "idle", Error: startupError}, nil
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

func isOllamaEmbeddingEndpoint(ctx context.Context, endpoint string) bool {
	server := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(endpoint), "/"), "/v1")
	return embeddingProviderName(endpoint) == "local" && probeOllamaServer(ctx, server).Catalog
}

func (c *localEmbeddingsController) Enable(ctx context.Context, builtin bool, report func(api.LocalEmbeddingsStatus)) (result error) {
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
	if !loaded.Vector.Embeddings.LocalOnly && !isOllamaEmbeddingEndpoint(ctx, loaded.Vector.Embeddings.Endpoint) && (loaded.Vector.Enabled || loaded.Vector.Embeddings.Endpoint != "" || loaded.Vector.Embeddings.Model != "") {
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
		if errors.Is(err, config.ErrConfigConflict) {
			return localEmbeddingsConflictError{cause: err}
		}
		return err
	}
	rebuildDocuments, err := c.localDocumentRebuildConsented(workCtx, &state)
	if err != nil {
		return err
	}
	var vf *vectorFeatures
	freshRuntime := c.handle.vf == nil
	if freshRuntime {
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
	publicationErr := publishTextVectorFeatures(nextCtx, c.store, c.server, c.scheduler, vf, &state)
	if freshRuntime {
		publishVisualVectorFeatures(c.store, c.server, c.scheduler, vf, &state)
	}
	defer func() {
		if rebuildDocuments {
			_, err := c.scheduler.StartJob("scheduled document indexing", false)
			publicationErr = errors.Join(publicationErr, err)
		}
		if c.refreshMCP != nil {
			publicationErr = errors.Join(publicationErr, c.refreshMCP(nextCtx, &state))
		}
		// A later message build cannot repair publication or scheduling failures.
		if publicationErr != nil {
			report(api.LocalEmbeddingsStatus{Phase: "idle"})
		}
		result = errors.Join(result, publicationErr)
	}()

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
		return localEmbeddingsCommandError(workCtx, args[1] == "build", err, stderr)
	}
	if building != nil && building.Fingerprint != fingerprint {
		if err := run("embeddings", "retire", strconv.FormatInt(int64(building.ID), 10), "--yes"); err != nil {
			return err
		}
		building = nil
	}
	args := []string{"embeddings", "build", "--yes", "--person-policy-config", loaded.ConfigFilePath()}
	if building == nil {
		active, err := vf.Backend.ActiveGeneration(ctx)
		if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
			return fmt.Errorf("read active index: %w", err)
		}
		if errors.Is(err, vector.ErrNoActiveGeneration) || active.Fingerprint != fingerprint {
			args = append(args, "--full-rebuild")
		}
	}
	return run(args...)
}

// Log the full command output, then surface only its final error line.
func localEmbeddingsCommandError(ctx context.Context, build bool, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if err == nil || detail == "" && !(build && errors.Is(err, context.Canceled)) {
		return err
	}
	loggerFromContext(ctx).Warn("Local search index command failed", "error", err, "stderr", detail)
	if index := strings.LastIndex("\n"+detail, "\nError: "); index >= 0 {
		detail = strings.TrimSpace(("\n" + detail)[index+len("\nError: "):])
	} else if index := strings.LastIndex(detail, "\n"); index >= 0 {
		detail = strings.TrimSpace(detail[index+1:])
	}
	// An interrupted build resumes from its saved progress.
	if build && (errors.Is(err, context.Canceled) || strings.HasSuffix(detail, context.Canceled.Error())) {
		return localEmbeddingsInterruptedError{cause: err}
	}
	if err.Error() == cliSubprocessExitSentinel {
		return errors.New(detail)
	}
	return fmt.Errorf("%w: %s", err, detail)
}

// Rebuild documents only when their selected destination already has exact consent.
func (c *localEmbeddingsController) localDocumentRebuildConsented(ctx context.Context, state *invocation) (bool, error) {
	if !state.cfg.Attachments.Documents.Index.Embeddings.Enabled {
		return false, nil
	}
	spec, err := configuredDocumentVectorSpec(ctx, c.store)
	if errors.Is(err, store.ErrDocumentVectorInvalidGenerationState) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hasDocumentVectorConsent(ctx, c.store, spec)
}
