package vector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestConfig_DefaultsAndParse(t *testing.T) {
	assert := assertpkg.New(t)
	input := `
enabled = true
backend = "sqlite-vec"
db_path = "/tmp/vectors.db"

[embeddings]
endpoint = "http://mac-studio.tailnet:8080/v1"
api_key_env = "MSGVAULT_EMBED_KEY"
model = "nomic-embed-text-v1.5"
dimension = 768
batch_size = 32
timeout = "15s"
max_retries = 2
max_input_chars = 16000

[preprocess]
strip_quotes = true
strip_signatures = true

[search]
rrf_k = 60
k_per_signal = 100
subject_boost = 2.0
max_page_size_hybrid = 50

[embed.schedule]
cron = "*/5 * * * *"
run_after_sync = true
`
	var c Config
	_, err := toml.Decode(input, &c)
	requirepkg.NoError(t, err, "decode")
	assert.True(c.Enabled, "Enabled should be true")
	assert.Equal("sqlite-vec", c.Backend)
	assert.Equal(768, c.Embeddings.Dimension)
	assert.Equal(15*time.Second, c.Embeddings.Timeout)
	assert.Equal(60, c.Search.RRFK)
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"OK", func(c *Config) {}, ""},
		{"MissingEndpoint", func(c *Config) { c.Embeddings.Endpoint = "" }, "endpoint"},
		{"InvalidEndpoint", func(c *Config) { c.Embeddings.Endpoint = "::not a url" }, "endpoint"},
		{"MissingScheme", func(c *Config) { c.Embeddings.Endpoint = "mac-studio:8080/v1" }, "endpoint"},
		{"UnsupportedScheme_FTP", func(c *Config) { c.Embeddings.Endpoint = "ftp://host/v1" }, "endpoint"},
		{"UnsupportedScheme_File", func(c *Config) { c.Embeddings.Endpoint = "file:///tmp/endpoint" }, "endpoint"},
		{"Hostless", func(c *Config) { c.Embeddings.Endpoint = "http:///v1" }, "endpoint"},
		{"HTTPS_OK", func(c *Config) { c.Embeddings.Endpoint = "https://host:8080/v1" }, ""},
		{"ZeroDim", func(c *Config) { c.Embeddings.Dimension = 0 }, "dimension"},
		{"NegativeDim", func(c *Config) { c.Embeddings.Dimension = -1 }, "dimension"},
		{"UnknownBackend", func(c *Config) { c.Backend = "mystery" }, "backend"},
		{"ZeroBatch", func(c *Config) { c.Embeddings.BatchSize = 0 }, "batch_size"},
		{"MissingModel", func(c *Config) { c.Embeddings.Model = "" }, "model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				requirepkg.NoError(t, err)
				return
			}
			requirepkg.Error(t, err, "expected error containing %q", tt.wantErr)
			assertpkg.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func validConfig() Config {
	return Config{
		Enabled: true,
		Backend: "sqlite-vec",
		DBPath:  "/tmp/v.db",
		Embeddings: EmbeddingsConfig{
			Endpoint:      "http://localhost:8080/v1",
			Model:         "nomic-embed-text",
			Dimension:     768,
			BatchSize:     32,
			Timeout:       10 * time.Second,
			MaxRetries:    2,
			MaxInputChars: 16000,
		},
		Search: SearchConfig{
			RRFK:              60,
			KPerSignal:        100,
			SubjectBoost:      2.0,
			MaxPageSizeHybrid: new(50),
		},
	}
}

// TestPreprocessConfig_Defaults covers the pointer-bool semantics: nil
// means "default true"; an explicit false in TOML must be preserved even
// when the sibling field is left unset.
func TestPreprocessConfig_Defaults(t *testing.T) {
	tests := []struct {
		name            string
		toml            string
		wantStripQuotes bool
		wantStripSig    bool
	}{
		{
			name:            "both_omitted",
			toml:            ``,
			wantStripQuotes: true,
			wantStripSig:    true,
		},
		{
			name: "both_explicit_true",
			toml: `
strip_quotes = true
strip_signatures = true
`,
			wantStripQuotes: true,
			wantStripSig:    true,
		},
		{
			name: "both_explicit_false",
			toml: `
strip_quotes = false
strip_signatures = false
`,
			wantStripQuotes: false,
			wantStripSig:    false,
		},
		{
			name: "quotes_false_signatures_omitted",
			toml: `
strip_quotes = false
`,
			wantStripQuotes: false,
			wantStripSig:    true,
		},
		{
			name: "signatures_false_quotes_omitted",
			toml: `
strip_signatures = false
`,
			wantStripQuotes: true,
			wantStripSig:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p PreprocessConfig
			_, err := toml.Decode(tt.toml, &p)
			requirepkg.NoError(t, err, "decode")
			assertpkg.Equal(t, tt.wantStripQuotes, p.StripQuotesEnabled())
			assertpkg.Equal(t, tt.wantStripSig, p.StripSignaturesEnabled())
		})
	}
}

// TestPreprocessConfig_NewToggleDefaults exercises the pointer-bool
// behavior of the four sanitization toggles added alongside the original
// strip_quotes / strip_signatures pair: each defaults to true when
// omitted, and an explicit false in TOML is preserved verbatim.
func TestPreprocessConfig_NewToggleDefaults(t *testing.T) {
	t.Run("all_omitted_default_true", func(t *testing.T) {
		assert := assertpkg.New(t)
		var p PreprocessConfig
		_, err := toml.Decode(``, &p)
		requirepkg.NoError(t, err, "decode")
		assert.True(p.StripHTMLEnabled())
		assert.True(p.StripBase64Enabled())
		assert.True(p.StripURLTrackingEnabled())
		assert.True(p.CollapseWhitespaceEnabled())
	})

	t.Run("all_explicit_false", func(t *testing.T) {
		assert := assertpkg.New(t)
		var p PreprocessConfig
		raw := `
strip_html = false
strip_base64 = false
strip_url_tracking = false
collapse_whitespace = false
`
		_, err := toml.Decode(raw, &p)
		requirepkg.NoError(t, err, "decode")
		assert.False(p.StripHTMLEnabled())
		assert.False(p.StripBase64Enabled())
		assert.False(p.StripURLTrackingEnabled())
		assert.False(p.CollapseWhitespaceEnabled())
	})

	t.Run("one_false_others_default_true", func(t *testing.T) {
		assert := assertpkg.New(t)
		var p PreprocessConfig
		_, err := toml.Decode(`strip_base64 = false`, &p)
		requirepkg.NoError(t, err, "decode")
		assert.False(p.StripBase64Enabled(), "StripBase64Enabled should be false (explicit)")
		assert.True(p.StripHTMLEnabled(), "omitted toggles should still default to true")
		assert.True(p.StripURLTrackingEnabled(), "omitted toggles should still default to true")
		assert.True(p.CollapseWhitespaceEnabled(), "omitted toggles should still default to true")
	})
}

// TestApplyDefaults_OverridesZeroValues verifies that zero-valued numeric
// fields get normalized to the documented defaults, so an omitted (or
// explicit 0) max_retries / timeout in TOML doesn't silently disable the
// underlying behavior.
func TestApplyDefaults_OverridesZeroValues(t *testing.T) {
	assert := assertpkg.New(t)
	c := Config{
		Backend:    "", // defaults to sqlite-vec
		Embeddings: EmbeddingsConfig{},
		// Preprocess intentionally left with nil pointers to confirm
		// ApplyDefaults doesn't clobber them.
		Preprocess: PreprocessConfig{
			StripQuotes: new(false), // explicit user intent
		},
	}
	c.ApplyDefaults()

	assert.Equal("sqlite-vec", c.Backend)
	assert.Equal(32, c.Embeddings.BatchSize)
	assert.Equal(30*time.Second, c.Embeddings.Timeout)
	assert.Equal(3, c.Embeddings.MaxRetries)
	assert.Equal(32768, c.Embeddings.MaxInputChars)
	assert.Equal(60, c.Search.RRFK)
	assert.Equal(100, c.Search.KPerSignal)
	assert.Equal(2.0, c.Search.SubjectBoost)
	if assert.NotNil(c.Search.MaxPageSizeHybrid, "Search.MaxPageSizeHybrid should be set") {
		assert.Equal(50, *c.Search.MaxPageSizeHybrid)
	}
	// Preprocess pointer must not be clobbered.
	assert.False(c.Preprocess.StripQuotesEnabled(), "user explicitly set false")
	assert.True(c.Preprocess.StripSignaturesEnabled(), "unset → default")
}

// TestApplyDefaults_PreservesExplicitMaxPageSizeHybridZero guards the
// "no clamp" sentinel: a user who explicitly sets
// `max_page_size_hybrid = 0` (an int* in TOML) wants to disable the
// per-request clamp, and ApplyDefaults must not silently rewrite that
// to 50. Repeated ApplyDefaults calls (Load() runs it twice) must not
// clobber the explicit zero either.
func TestApplyDefaults_PreservesExplicitMaxPageSizeHybridZero(t *testing.T) {
	c := Config{
		Search: SearchConfig{MaxPageSizeHybrid: new(0)},
	}
	c.ApplyDefaults()
	c.ApplyDefaults() // idempotent: second call must not clobber
	assertpkg.Equal(t, 0, c.Search.MaxPageSizeHybridClamp(), "explicit user disable")
}

func TestEmbeddingsConfig_ETAWindowDefault(t *testing.T) {
	var c Config
	c.Embeddings.Endpoint = "http://localhost:1234/v1"
	c.ApplyDefaults()
	requirepkg.Equal(t, 10, c.Embeddings.ETAWindow, "ETAWindow default")
}

func TestEmbeddingsConfig_ETAWindowExplicit(t *testing.T) {
	var c Config
	c.Embeddings.Endpoint = "http://localhost:1234/v1"
	c.Embeddings.ETAWindow = 25
	c.ApplyDefaults()
	requirepkg.Equal(t, 25, c.Embeddings.ETAWindow, "ETAWindow explicit")
}

// TestSearchConfig_PointerSemantics_FromTOML rounds out the
// pointer-semantic guarantee at the TOML decode layer: omitted →
// nil → ApplyDefaults fills 50; explicit 0 → preserved; explicit
// positive → preserved.
func TestSearchConfig_PointerSemantics_FromTOML(t *testing.T) {
	cases := []struct {
		name      string
		tomlInput string
		want      int
	}{
		{"omitted_defaults_to_50", `[search]`, 50},
		{"explicit_zero_disables_clamp", "[search]\nmax_page_size_hybrid = 0", 0},
		{"explicit_positive_preserved", "[search]\nmax_page_size_hybrid = 200", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			_, err := toml.Decode(tc.tomlInput, &c)
			requirepkg.NoError(t, err, "decode")
			c.ApplyDefaults()
			assertpkg.Equal(t, tc.want, c.Search.MaxPageSizeHybridClamp())
		})
	}
}

// TestPreprocessConfig_FingerprintFormat pins the human-readable format
// (p<ver>-<flags>) so the generation-fingerprint string in stats output
// and DB rows is stable for operators.
func TestPreprocessConfig_FingerprintFormat(t *testing.T) {
	allOn := PreprocessConfig{}
	assertpkg.Equal(t, "p1-111111", allOn.Fingerprint(), "default Fingerprint()")

	f := false
	allOff := PreprocessConfig{
		StripQuotes:        &f,
		StripSignatures:    &f,
		StripHTML:          &f,
		StripBase64:        &f,
		StripURLTracking:   &f,
		CollapseWhitespace: &f,
	}
	assertpkg.Equal(t, "p1-000000", allOff.Fingerprint(), "all-off Fingerprint()")
}

// TestPreprocessConfig_FingerprintChangesPerToggle ensures every toggle
// participates in the fingerprint so flipping any one of them stales
// the index. Without per-toggle coverage a future refactor could
// accidentally drop one from the hash and re-introduce the silent
// mid-generation policy drift this fingerprint was built to prevent.
func TestPreprocessConfig_FingerprintChangesPerToggle(t *testing.T) {
	baseline := PreprocessConfig{}.Fingerprint()
	f := false
	cases := []struct {
		name string
		cfg  PreprocessConfig
	}{
		{"strip_quotes", PreprocessConfig{StripQuotes: &f}},
		{"strip_signatures", PreprocessConfig{StripSignatures: &f}},
		{"strip_html", PreprocessConfig{StripHTML: &f}},
		{"strip_base64", PreprocessConfig{StripBase64: &f}},
		{"strip_url_tracking", PreprocessConfig{StripURLTracking: &f}},
		{"collapse_whitespace", PreprocessConfig{CollapseWhitespace: &f}},
	}
	seen := map[string]string{baseline: "all-on baseline"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Fingerprint()
			assertpkg.NotEqual(t, baseline, got, "Fingerprint() with %s=false should differ from baseline", tc.name)
			if other, dup := seen[got]; dup {
				assertpkg.Failf(t, "Fingerprint collision",
					"Fingerprint() with %s=false = %q, collides with %s",
					tc.name, got, other)
			}
			seen[got] = tc.name
		})
	}
}

// TestConfig_GenerationFingerprintFolds checks the full identifier
// composition. The shape is
// "<model>:<dim>:<preprocess>:c<maxchars>:e<embed_policy>" — every
// segment must contribute so an operator switching the model, any
// preprocess toggle, the truncation cap, or the embed-worker output
// layout stales the existing index instead of silently mixing
// inconsistently-prepared vectors.
func TestConfig_GenerationFingerprintFolds(t *testing.T) {
	base := Config{
		Embeddings: EmbeddingsConfig{Model: "nomic-embed", Dimension: 768, MaxInputChars: 6000},
	}
	got := base.GenerationFingerprint()
	want := fmt.Sprintf("nomic-embed:768:p1-111111:c6000:e%d", embedPolicyVersion)
	assertpkg.Equal(t, want, got)

	// Flipping the model invalidates.
	modelChanged := base
	modelChanged.Embeddings.Model = "snowflake-arctic"
	assertpkg.NotEqual(t, got, modelChanged.GenerationFingerprint(), "GenerationFingerprint should change when Model changes")

	// Flipping a preprocess toggle invalidates too — this is the gap
	// the reviewer flagged in round one. Without folding Preprocess
	// into the fingerprint, the active generation would keep absorbing
	// new vectors built under a different sanitization policy.
	f := false
	preprocessChanged := base
	preprocessChanged.Preprocess.StripHTML = &f
	assertpkg.NotEqual(t, got, preprocessChanged.GenerationFingerprint(), "GenerationFingerprint should change when strip_html flips to false")
}

// TestConfig_GenerationFingerprint_IncludesMaxInputChars pins the gap
// flagged in roborev round two: MaxInputChars is the rune-bounded
// truncation cap fed straight into Preprocess() by the embed worker,
// so changing it produces a different embedded string for any message
// whose preprocessed form exceeds either the old or new cap. Two
// different cap values therefore produce two different embedding
// spaces and must not share one active generation.
func TestConfig_GenerationFingerprint_IncludesMaxInputChars(t *testing.T) {
	base := Config{
		Embeddings: EmbeddingsConfig{Model: "m", Dimension: 8, MaxInputChars: 6000},
	}
	baseline := base.GenerationFingerprint()

	bumped := base
	bumped.Embeddings.MaxInputChars = 12000
	assertpkg.NotEqual(t, baseline, bumped.GenerationFingerprint(),
		"GenerationFingerprint should change when MaxInputChars goes 6000→12000")

	// The zero-cap case (Preprocess treats <=0 as "no truncation")
	// must also be distinguishable from any positive cap, otherwise
	// disabling truncation later wouldn't stale a generation that had
	// always been truncating.
	zeroed := base
	zeroed.Embeddings.MaxInputChars = 0
	assertpkg.NotEqual(t, baseline, zeroed.GenerationFingerprint(),
		"GenerationFingerprint should change when MaxInputChars goes 6000→0")
}

// TestConfig_GenerationFingerprint_IncludesEmbedPolicyVersion pins the
// trailing :e<embedPolicyVersion> segment. The embed worker switched
// from one-vector-per-message-with-truncation to N-vectors-per-message-
// via-ChunkText; without this segment an active generation seeded
// under the old single-vector policy would silently accept new chunked
// entries from an upgraded worker, mixing two incompatible vector
// layouts inside one generation.
func TestConfig_GenerationFingerprint_IncludesEmbedPolicyVersion(t *testing.T) {
	base := Config{
		Embeddings: EmbeddingsConfig{Model: "m", Dimension: 8, MaxInputChars: 6000},
	}
	got := base.GenerationFingerprint()
	suffix := fmt.Sprintf(":e%d", embedPolicyVersion)
	assertpkg.True(t, strings.HasSuffix(got, suffix), "GenerationFingerprint() = %q, want suffix %q", got, suffix)
}
