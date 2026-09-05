package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/voyage"

	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/visual"
)

var (
	multimodalProbeSeeds    string
	multimodalProbeFixtures string
	multimodalProbeOut      string
	multimodalProbeYes      bool
)

// multimodalProbeCmd runs the authenticated Voyage capability probe locally.
// It never goes through the daemon: the manifest is a local file the operator
// reviews and then configures as vector.multimodal.capabilities_file.
var multimodalProbeCmd = &cobra.Command{
	Use:   "probe",
	Short: "Probe Voyage capabilities and write the manifest",
	Long: `Runs the authenticated docbank capability probe against Voyage using
private synthetic fixtures, and writes the sanitized capability manifest.

The probe uploads only deterministic synthetic media: generated JPEG, PNG,
and GIF fixtures plus the operator-supplied synthetic WebP and MP4 seeds
(a primary and a contrasting variant of each).
No archive content leaves the machine. The manifest records pass, reject,
and fail observations per capability and contains no media or vectors.

Review the manifest, set vector.multimodal.capabilities_file to its path,
then run 'msgvault multimodal build --yes' to consent to exactly that
capability profile.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(multimodalProbeSeeds) == "" {
			return usageErr(cmd, errors.New("--seeds is required: a private directory holding synthetic image_webp.webp, image_webp_alt.webp, video_mp4.mp4, and video_mp4_alt.mp4 seeds"))
		}
		if strings.TrimSpace(multimodalProbeOut) == "" {
			return usageErr(cmd, errors.New("--out is required"))
		}
		// The lane may not be enabled yet (probing precedes enablement), so
		// Config.Validate has not covered these settings. Confirm the provider
		// and pinned destination before reading the credential: a key meant
		// for some other configured endpoint must never reach Voyage.
		if err := cfg.Vector.Multimodal.Validate(); err != nil {
			return err
		}
		credentials, err := providercredentials.Read(cfg.TokensDir())
		if err != nil {
			return fmt.Errorf("load provider credentials: %w", err)
		}
		apiKey, err := resolveProviderCredentialFromSnapshot(
			credentials, providercredentials.VectorMultimodalID,
			cfg.Vector.Multimodal.Endpoint, cfg.Vector.Multimodal.APIKeyEnv,
		)
		if err != nil {
			return fmt.Errorf("resolve visual embedding credential: %w", err)
		}
		if apiKey == "" {
			return fmt.Errorf("visual embedding credential is not configured: store one for %s in Settings "+
				"or set environment variable %s", providercredentials.VectorMultimodalID, cfg.Vector.Multimodal.APIKeyEnv)
		}
		if !multimodalProbeYes {
			return usageErr(cmd, errors.New("the probe sends synthetic fixture media to the configured provider; pass --yes to continue"))
		}
		policy, err := voyage.NewPolicy(voyage.PolicyConfig{
			Model:     cfg.Vector.Multimodal.Model,
			Dimension: cfg.Vector.Multimodal.Dimension,
		})
		if err != nil {
			return fmt.Errorf("voyage policy: %w", err)
		}
		fixtureDir := multimodalProbeFixtures
		if fixtureDir == "" {
			parent, err := os.MkdirTemp("", "msgvault-voyage-probe-*")
			if err != nil {
				return fmt.Errorf("create probe fixture directory: %w", err)
			}
			defer func() { _ = os.RemoveAll(parent) }()
			fixtureDir = filepath.Join(parent, "fixtures")
		}
		ctx := cmd.Context()
		if err := voyage.WriteProbeFixtures(ctx, fixtureDir, voyage.FixtureOptions{
			SeedDirectory: multimodalProbeSeeds,
		}); err != nil {
			return fmt.Errorf("write probe fixtures: %w", err)
		}
		fixtures := voyage.ProbeFixtureConfig{FixtureDirectory: fixtureDir}
		if err := voyage.ValidateProbeFixtures(ctx, policy, fixtures); err != nil {
			return fmt.Errorf("validate probe fixtures: %w", err)
		}
		client, err := voyage.NewClient(policy, voyage.ClientConfig{
			APIKey: apiKey, HTTPClient: providerHTTPClientWithoutRedirects(nil),
		})
		if err != nil {
			return fmt.Errorf("voyage client: %w", err)
		}
		manifest, err := voyage.RunCapabilityProbe(ctx, client, voyage.ProbeConfig{Fixtures: fixtures})
		if err != nil {
			return fmt.Errorf("run capability probe: %w", err)
		}
		if err := writeVisualCapabilityManifest(multimodalProbeOut, manifest); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprintf(out, "Capability manifest written to %s\n", multimodalProbeOut); err != nil {
			return fmt.Errorf("write probe summary: %w", err)
		}
		for _, result := range manifest.Results {
			line := string(result.Status)
			if result.ReasonCode != "" {
				line += " (" + result.ReasonCode + ")"
			}
			if _, err := fmt.Fprintf(out, "  %-30s %s\n", result.CapabilityID, line); err != nil {
				return fmt.Errorf("write probe summary: %w", err)
			}
		}
		return nil
	},
}

func writeVisualCapabilityManifest(path string, manifest voyage.CapabilityManifest) error {
	if err := fileutil.SecureMkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create capability manifest (refusing to overwrite an existing file): %w", err)
	}
	encodeErr := voyage.EncodeCapabilityManifest(file, manifest)
	closeErr := file.Close()
	if err := errors.Join(encodeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write capability manifest: %w", err)
	}
	return nil
}

// visualVoyageConfig binds the manifest to the media policy used by both
// runtime uploads and setup's read-only consent check.
func visualVoyageConfig(cfg vector.Config) (visual.VoyageConfig, error) {
	manifest, err := loadVisualCapabilityManifest(cfg.Multimodal.CapabilitiesFile)
	if err != nil {
		return visual.VoyageConfig{}, err
	}
	media := visual.DefaultMediaPolicy()
	media.IncludeImages = cfg.Multimodal.ImagesEnabled() || cfg.Multimodal.ImageQueriesEnabled()
	media.IncludeVideo = cfg.Multimodal.VideoEnabled()
	media.AllowAnimatedGIF = cfg.Multimodal.AnimatedGIFsEnabled()
	provider := visual.VoyageConfig{
		Model: cfg.Multimodal.Model, Dimension: cfg.Multimodal.Dimension,
		Manifest: manifest, Media: media,
	}
	policy, err := provider.Policy()
	if err != nil {
		return visual.VoyageConfig{}, fmt.Errorf("configure visual capability policy: %w", err)
	}
	// Every visual search embeds its text query through this provider. A
	// manifest that only authorizes indexing cannot make the lane usable.
	if _, err := policy.Authorize(manifest, voyage.CapabilityQueryText); err != nil {
		return visual.VoyageConfig{}, fmt.Errorf("capability manifest does not authorize text queries; re-run `msgvault multimodal probe`: %w", err)
	}
	return provider, nil
}

// loadVisualCapabilityManifest reads and strictly validates the operator's
// probed Voyage capability manifest. The multimodal lane cannot run without
// one: nothing has upload authority until a probe recorded it.
func loadVisualCapabilityManifest(path string) (voyage.CapabilityManifest, error) {
	if strings.TrimSpace(path) == "" {
		return voyage.CapabilityManifest{}, errors.New(
			"vector.multimodal.capabilities_file is not set; run `msgvault multimodal probe` and configure the manifest path")
	}
	file, err := os.Open(path)
	if err != nil {
		return voyage.CapabilityManifest{}, fmt.Errorf("open Voyage capability manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	manifest, err := voyage.DecodeCapabilityManifest(file)
	if err != nil {
		return voyage.CapabilityManifest{}, fmt.Errorf("decode Voyage capability manifest %s: %w", path, err)
	}
	return manifest, nil
}

func init() {
	multimodalProbeCmd.Flags().StringVar(&multimodalProbeSeeds, "seeds", "", "Private directory with synthetic WebP and MP4 seeds (primary and contrasting variant of each)")
	multimodalProbeCmd.Flags().StringVar(&multimodalProbeFixtures, "fixtures", "", "Optional directory to keep the generated fixtures (default: temporary)")
	multimodalProbeCmd.Flags().StringVar(&multimodalProbeOut, "out", "", "Path to write the capability manifest")
	multimodalProbeCmd.Flags().BoolVar(&multimodalProbeYes, "yes", false, "Confirm sending synthetic fixtures to the provider")
	multimodalCmd.AddCommand(multimodalProbeCmd)
}
