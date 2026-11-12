package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type localEmbeddingsController struct {
	gate         api.OperationGate
	state        *invocation
	store        *store.Store
	dbPath       string
	handle       *vectorInitHandle
	server       *api.Server
	scheduler    *scheduler.Scheduler
	runner       api.CLIRunner
	openers      []visual.StreamOpener
	inspect      func(context.Context, embedlocal.Options) (embedlocal.Status, error)
	install      func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error)
	choice       func() (bool, error)
	recordChoice func() error
}

type localEmbeddingsConflictError struct{ cause error }

func (localEmbeddingsConflictError) Error() string {
	return "Settings changed during setup. Turn on to retry."
}

func (e localEmbeddingsConflictError) Unwrap() error {
	return e.cause
}

func (c *localEmbeddingsController) Inspect(ctx context.Context) (api.LocalEmbeddingsStatus, error) {
	if _, unavailable := setupVectorBackend(c.state.cfg); unavailable != "" {
		return api.LocalEmbeddingsStatus{}, errors.New(unavailable)
	}
	c.handle.mu.Lock()
	var cfg vector.Config
	missingRuntime := c.handle.vf == nil
	if c.handle.vf != nil {
		cfg = c.handle.vf.Cfg
	}
	c.handle.mu.Unlock()
	if missingRuntime {
		snapshot, err := config.ReadConfigFile(c.state.cfg.ConfigFilePath())
		if err != nil {
			return api.LocalEmbeddingsStatus{}, err
		}
		loaded, err := c.state.cfg.ReloadConfigFile(snapshot)
		if err != nil {
			return api.LocalEmbeddingsStatus{}, err
		}
		cfg = loaded.Vector
	}
	configured := textProviderConfigured(cfg)
	if missingRuntime {
		cfg.Enabled = false
	}
	dir, err := localEmbeddingsDir()
	if err != nil {
		return api.LocalEmbeddingsStatus{}, err
	}
	status, err := c.inspect(ctx, embedlocal.Options{DataDir: dir, SkipOllama: configured})
	if err != nil {
		return api.LocalEmbeddingsStatus{}, err
	}
	provider := ""
	if configured {
		switch {
		case cfg.Embeddings.LocalOnly:
			provider = "builtin"
		case cfg.Enabled && isOllamaEmbeddingEndpoint(ctx, cfg.Embeddings.Endpoint):
			provider = "ollama"
		default:
			provider = "configured"
		}
	}
	if provider == "" && status.OllamaAvailable {
		provider = "ollama"
	}
	return api.LocalEmbeddingsStatus{Enabled: cfg.Enabled, Provider: provider,
		DownloadBytes: status.DownloadBytes, Phase: "idle"}, nil
}

func textProviderConfigured(cfg vector.Config) bool {
	return cfg.Enabled || cfg.Embeddings.LocalOnly || cfg.Embeddings.Endpoint != "" || cfg.Embeddings.Model != ""
}

func isOllamaEmbeddingEndpoint(ctx context.Context, endpoint string) bool {
	server := strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(endpoint), "/"), "/embeddings"), "/v1")
	return embeddingProviderName(endpoint) == "local" && probeOllamaServer(ctx, server).Catalog
}

func (c *localEmbeddingsController) Enable(ctx context.Context, report func(api.LocalEmbeddingsStatus)) (result error) {
	return c.enable(ctx, report, false)
}

func (c *localEmbeddingsController) Start(ctx context.Context, report func(api.LocalEmbeddingsStatus)) error {
	if store.IsPostgresURL(c.dbPath) {
		return nil
	}
	if _, unavailable := setupVectorBackend(c.state.cfg); unavailable != "" {
		return nil
	}
	chosen, err := c.choice()
	if err != nil || !chosen {
		return err
	}
	snapshot, err := config.ReadConfigFile(c.state.cfg.ConfigFilePath())
	if err != nil {
		return err
	}
	loaded, err := c.state.cfg.ReloadConfigFile(snapshot)
	if err != nil {
		return err
	}
	if textProviderConfigured(loaded.Vector) {
		return nil
	}
	dir, err := localEmbeddingsDir()
	if err != nil {
		return err
	}
	status, err := c.inspect(ctx, embedlocal.Options{DataDir: dir, SkipOllama: true})
	if err != nil || status.DownloadBytes != 0 {
		return err
	}
	return c.enable(ctx, report, true)
}

func (c *localEmbeddingsController) enable(ctx context.Context, report func(api.LocalEmbeddingsStatus), startup bool) (result error) {
	// Bound accepted setup work independently of the HTTP request.
	runtimeCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	if !c.handle.WaitContext(ctx) {
		return ctx.Err()
	}
	c.handle.mu.Lock()
	live := c.handle.vf
	c.handle.mu.Unlock()
	if startup && live == nil {
		if status, detail := c.server.VectorStatus(); status == api.VectorStatusError {
			return errors.New(detail)
		}
	}

	snapshot, err := config.ReadConfigFile(c.state.cfg.ConfigFilePath())
	if err != nil {
		return err
	}
	loaded, err := c.state.cfg.ReloadConfigFile(snapshot)
	if err == nil && !loaded.Vector.Enabled && loaded.Vector.Embeddings.LocalOnly {
		err = loaded.Vector.Embeddings.Validate() // Keep a saved custom model instead of replacing it with Nomic.
	}
	if err != nil {
		return err
	}
	if startup && textProviderConfigured(loaded.Vector) {
		return nil
	}
	dir, err := localEmbeddingsDir()
	if err != nil {
		return err
	}
	configKeys, err := toml.Decode(string(snapshot.Content), &map[string]any{})
	if err != nil {
		return fmt.Errorf("read local search settings: %w", err)
	}
	deps := defaultSetupProvidersDeps(ctx)
	deps.lookupEnv = func(string) (string, bool) { return "", false }
	deps.localEndpoint = localEmbeddingEndpoint
	detection := detectSetupProviders(ctx, loaded, deps, setupProvidersOptions{localEmbeddings: startup || (!loaded.Vector.Enabled && loaded.Vector.Embeddings.LocalOnly)})
	detection.configKeys = configKeys
	lane := planTextSearch(loaded, detection)
	if lane.Action == planActionKeep {
		if status, err := c.Inspect(ctx); err == nil && status.Enabled && status.Provider != "configured" {
			return nil
		}
	}
	if lane.Action != planActionEnable {
		return errors.New(lane.Reason)
	}
	candidate, err := config.PreviewConfigTableEdits(snapshot, lane.edits)
	if err != nil {
		return err
	}
	nextConfig, err := c.state.cfg.ReloadConfigFile(candidate)
	if err != nil {
		return err
	}
	if nextConfig.DatabaseDSN() != c.dbPath || nextConfig.Data.DataDir != c.state.cfg.Data.DataDir ||
		(live != nil && nextConfig.Vector.DBPath != live.Cfg.DBPath) {
		return errors.New("Restart msgvault to use the new storage settings.") //nolint:revive,staticcheck // Displayed directly as a recovery message in Settings.
	}
	var builtinEndpoint string
	if lane.local && !startup {
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
	releaseGate := func() {}
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
		releaseGate = release
	}
	defer func() { releaseGate() }()
	// Apply saved text settings together while preserving the live image lane.
	runtimeConfig := *c.state.cfg
	if live != nil {
		runtimeConfig.Vector = live.Cfg
	}
	multimodal := runtimeConfig.Vector.Multimodal
	runtimeConfig.Vector = nextConfig.Vector
	runtimeConfig.Vector.Multimodal = multimodal
	state := *c.state
	state.cfg = &runtimeConfig
	nextCtx := withInvocation(runtimeCtx, &state)
	workCtx := withInvocation(ctx, &state)
	var vf *vectorFeatures
	if live == nil {
		vf, err = setupVectorFeaturesForRun(nextCtx, c.store, c.dbPath, false, c.openers...)
	} else {
		vecCfg, resolveErr := resolvedVectorConfig(workCtx, c.store, runtimeConfig.Vector)
		if resolveErr != nil {
			return resolveErr
		}
		if lane.local && startup {
			vecCfg, err = resolveLocalVectorConfig(workCtx, vecCfg)
			if err != nil {
				return err
			}
		} else if lane.local {
			vecCfg.Embeddings.Endpoint = builtinEndpoint
		}
		credentialSnapshot, credentialErr := providercredentials.Read(state.cfg.TokensDir())
		if credentialErr != nil {
			return fmt.Errorf("load text embedding credentials: %w", credentialErr)
		}
		vf, err = live.configureText(nextCtx, &state, vecCfg, credentialSnapshot)
	}
	if err != nil {
		return err
	}
	var activeExists, buildingExists bool
	if startup {
		closeNew := func() {
			if live == nil {
				_ = vf.Close()
			}
		}
		active, activeErr := vf.Backend.ActiveGeneration(ctx)
		if activeErr != nil && !errors.Is(activeErr, vector.ErrNoActiveGeneration) {
			closeNew()
			return fmt.Errorf("read active index: %w", activeErr)
		}
		building, buildingErr := vf.Backend.BuildingGeneration(ctx)
		if buildingErr != nil {
			closeNew()
			return fmt.Errorf("read unfinished index: %w", buildingErr)
		}
		activeExists, buildingExists = activeErr == nil, building != nil
		fingerprint := vf.Cfg.GenerationFingerprint()
		if (activeExists && active.Fingerprint != fingerprint) || (buildingExists && building.Fingerprint != fingerprint) {
			closeNew()
			return errors.New("An existing search index uses a different model. Turn on search in Settings to rebuild it.") //nolint:revive,staticcheck // Displayed in Settings.
		}
		latest, readErr := config.ReadConfigFile(c.state.cfg.ConfigFilePath())
		if readErr != nil {
			closeNew()
			return readErr
		}
		current, readErr := c.state.cfg.ReloadConfigFile(latest)
		if readErr != nil {
			closeNew()
			return readErr
		}
		if textProviderConfigured(current.Vector) || current.DatabaseDSN() != c.dbPath || current.Data.DataDir != c.state.cfg.Data.DataDir || current.Vector.DBPath != loaded.Vector.DBPath {
			closeNew()
			return errors.New("Settings changed during setup. Restart msgvault to apply them.") //nolint:revive,staticcheck // Displayed in Settings.
		}
	}
	if !startup {
		if _, err := config.EditConfigTables(loaded.ConfigFilePath(), snapshot.ETag, lane.edits); err != nil {
			if live == nil {
				_ = vf.Close()
			}
			if errors.Is(err, config.ErrConfigConflict) {
				return localEmbeddingsConflictError{cause: err}
			}
			return err
		}
	}
	c.handle.mu.Lock()
	c.handle.vf = vf
	c.handle.mu.Unlock()
	publicationErr := publishTextVectorFeatures(nextCtx, c.store, c.server, c.scheduler, vf, &state)
	if live == nil {
		publishVisualVectorFeatures(c.store, c.server, c.scheduler, vf, &state)
	}
	defer func() { result = errors.Join(result, publicationErr) }()
	if publicationErr == nil && !startup && lane.local && c.recordChoice != nil {
		if choiceErr := c.recordChoice(); choiceErr != nil {
			loggerFromContext(ctx).Warn("Could not record local search choice", "error", choiceErr)
		}
	}
	releaseGate()
	releaseGate = func() {}

	report(api.LocalEmbeddingsStatus{Phase: "indexing"})
	fingerprint := vf.Cfg.GenerationFingerprint()
	run := func(args ...string) error {
		stderr := &childStderr{w: io.Discard}
		err := c.runner.RunCLICommand(c.server.WithPublishedVectorConfig(workCtx), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
			if event.Type == cliStreamStderr {
				_, err := stderr.Write([]byte(event.Data))
				return err
			}
			return nil
		})
		return localEmbeddingsCommandError(workCtx, err, string(stderr.tail))
	}
	args := []string{"embeddings", "build", "--backstop", "--yes"}
	active, err := vf.Backend.ActiveGeneration(ctx)
	if err != nil && !errors.Is(err, vector.ErrNoActiveGeneration) {
		return fmt.Errorf("read active index: %w", err)
	}
	if (startup && !activeExists && !buildingExists) || (!startup && (errors.Is(err, vector.ErrNoActiveGeneration) || active.Fingerprint != fingerprint)) {
		args = append(args, "--full-rebuild")
	}
	return run(args...)
}

// Log the full command output, then surface only its final error line.
func localEmbeddingsCommandError(ctx context.Context, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if err == nil || detail == "" {
		return err
	}
	loggerFromContext(ctx).Warn("Local search index command failed", "error", err, "stderr", detail)
	if index := strings.LastIndex("\n"+detail, "\nError: "); index >= 0 {
		detail = strings.TrimSpace(("\n" + detail)[index+len("\nError: "):])
	} else if index := strings.LastIndex(detail, "\n"); index >= 0 {
		detail = strings.TrimSpace(detail[index+1:])
	}
	if err.Error() == cliSubprocessExitSentinel {
		return errors.New(detail)
	}
	return fmt.Errorf("%w: %s", err, detail)
}
