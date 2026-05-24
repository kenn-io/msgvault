package vector

import (
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

func TestConfig_DefaultsAndParse(t *testing.T) {
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
	if _, err := toml.Decode(input, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !c.Enabled {
		t.Fatal("Enabled should be true")
	}
	if c.Backend != "sqlite-vec" {
		t.Errorf("Backend=%q, want sqlite-vec", c.Backend)
	}
	if c.Embeddings.Dimension != 768 {
		t.Errorf("Dimension=%d, want 768", c.Embeddings.Dimension)
	}
	if c.Embeddings.Timeout != 15*time.Second {
		t.Errorf("Timeout=%v, want 15s", c.Embeddings.Timeout)
	}
	if c.Search.RRFK != 60 {
		t.Errorf("RRFK=%d, want 60", c.Search.RRFK)
	}
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
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q missing %q", err.Error(), tt.wantErr)
			}
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
			MaxPageSizeHybrid: intPtr(50),
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
			if _, err := toml.Decode(tt.toml, &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := p.StripQuotesEnabled(); got != tt.wantStripQuotes {
				t.Errorf("StripQuotesEnabled() = %v, want %v", got, tt.wantStripQuotes)
			}
			if got := p.StripSignaturesEnabled(); got != tt.wantStripSig {
				t.Errorf("StripSignaturesEnabled() = %v, want %v", got, tt.wantStripSig)
			}
		})
	}
}

// TestPreprocessConfig_NewToggleDefaults exercises the pointer-bool
// behavior of the four sanitization toggles added alongside the original
// strip_quotes / strip_signatures pair: each defaults to true when
// omitted, and an explicit false in TOML is preserved verbatim.
func TestPreprocessConfig_NewToggleDefaults(t *testing.T) {
	t.Run("all_omitted_default_true", func(t *testing.T) {
		var p PreprocessConfig
		if _, err := toml.Decode(``, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !p.StripHTMLEnabled() {
			t.Error("StripHTMLEnabled() = false, want true")
		}
		if !p.StripBase64Enabled() {
			t.Error("StripBase64Enabled() = false, want true")
		}
		if !p.StripURLTrackingEnabled() {
			t.Error("StripURLTrackingEnabled() = false, want true")
		}
		if !p.CollapseWhitespaceEnabled() {
			t.Error("CollapseWhitespaceEnabled() = false, want true")
		}
	})

	t.Run("all_explicit_false", func(t *testing.T) {
		var p PreprocessConfig
		raw := `
strip_html = false
strip_base64 = false
strip_url_tracking = false
collapse_whitespace = false
`
		if _, err := toml.Decode(raw, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if p.StripHTMLEnabled() {
			t.Error("StripHTMLEnabled() = true, want false")
		}
		if p.StripBase64Enabled() {
			t.Error("StripBase64Enabled() = true, want false")
		}
		if p.StripURLTrackingEnabled() {
			t.Error("StripURLTrackingEnabled() = true, want false")
		}
		if p.CollapseWhitespaceEnabled() {
			t.Error("CollapseWhitespaceEnabled() = true, want false")
		}
	})

	t.Run("one_false_others_default_true", func(t *testing.T) {
		var p PreprocessConfig
		if _, err := toml.Decode(`strip_base64 = false`, &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if p.StripBase64Enabled() {
			t.Error("StripBase64Enabled() should be false (explicit)")
		}
		if !p.StripHTMLEnabled() || !p.StripURLTrackingEnabled() || !p.CollapseWhitespaceEnabled() {
			t.Error("omitted toggles should still default to true")
		}
	})
}

// TestApplyDefaults_OverridesZeroValues verifies that zero-valued numeric
// fields get normalized to the documented defaults, so an omitted (or
// explicit 0) max_retries / timeout in TOML doesn't silently disable the
// underlying behavior.
func TestApplyDefaults_OverridesZeroValues(t *testing.T) {
	c := Config{
		Backend:    "", // defaults to sqlite-vec
		Embeddings: EmbeddingsConfig{},
		// Preprocess intentionally left with nil pointers to confirm
		// ApplyDefaults doesn't clobber them.
		Preprocess: PreprocessConfig{
			StripQuotes: boolPtr(false), // explicit user intent
		},
	}
	c.ApplyDefaults()

	if c.Backend != "sqlite-vec" {
		t.Errorf("Backend = %q, want sqlite-vec", c.Backend)
	}
	if c.Embeddings.BatchSize != 32 {
		t.Errorf("BatchSize = %d, want 32", c.Embeddings.BatchSize)
	}
	if c.Embeddings.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", c.Embeddings.Timeout)
	}
	if c.Embeddings.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", c.Embeddings.MaxRetries)
	}
	if c.Embeddings.MaxInputChars != 32768 {
		t.Errorf("MaxInputChars = %d, want 32768", c.Embeddings.MaxInputChars)
	}
	if c.Search.RRFK != 60 {
		t.Errorf("Search.RRFK = %d, want 60", c.Search.RRFK)
	}
	if c.Search.KPerSignal != 100 {
		t.Errorf("Search.KPerSignal = %d, want 100", c.Search.KPerSignal)
	}
	if c.Search.SubjectBoost != 2.0 {
		t.Errorf("Search.SubjectBoost = %v, want 2.0", c.Search.SubjectBoost)
	}
	if c.Search.MaxPageSizeHybrid == nil || *c.Search.MaxPageSizeHybrid != 50 {
		t.Errorf("Search.MaxPageSizeHybrid = %v, want pointer to 50", c.Search.MaxPageSizeHybrid)
	}
	// Preprocess pointer must not be clobbered.
	if c.Preprocess.StripQuotesEnabled() != false {
		t.Errorf("StripQuotesEnabled() = %v, want false (user explicitly set)", c.Preprocess.StripQuotesEnabled())
	}
	if c.Preprocess.StripSignaturesEnabled() != true {
		t.Errorf("StripSignaturesEnabled() = %v, want true (unset → default)", c.Preprocess.StripSignaturesEnabled())
	}
}

// TestApplyDefaults_PreservesExplicitMaxPageSizeHybridZero guards the
// "no clamp" sentinel: a user who explicitly sets
// `max_page_size_hybrid = 0` (an int* in TOML) wants to disable the
// per-request clamp, and ApplyDefaults must not silently rewrite that
// to 50. Repeated ApplyDefaults calls (Load() runs it twice) must not
// clobber the explicit zero either.
func TestApplyDefaults_PreservesExplicitMaxPageSizeHybridZero(t *testing.T) {
	c := Config{
		Search: SearchConfig{MaxPageSizeHybrid: intPtr(0)},
	}
	c.ApplyDefaults()
	c.ApplyDefaults() // idempotent: second call must not clobber
	if got := c.Search.MaxPageSizeHybridClamp(); got != 0 {
		t.Errorf("MaxPageSizeHybridClamp() = %d, want 0 (explicit user disable)", got)
	}
}

func TestEmbeddingsConfig_ETAWindowDefault(t *testing.T) {
	var c Config
	c.Embeddings.Endpoint = "http://localhost:1234/v1"
	c.ApplyDefaults()
	if c.Embeddings.ETAWindow != 10 {
		t.Fatalf("ETAWindow default: got %d, want 10", c.Embeddings.ETAWindow)
	}
}

func TestEmbeddingsConfig_ETAWindowExplicit(t *testing.T) {
	var c Config
	c.Embeddings.Endpoint = "http://localhost:1234/v1"
	c.Embeddings.ETAWindow = 25
	c.ApplyDefaults()
	if c.Embeddings.ETAWindow != 25 {
		t.Fatalf("ETAWindow explicit: got %d, want 25", c.Embeddings.ETAWindow)
	}
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
			if _, err := toml.Decode(tc.tomlInput, &c); err != nil {
				t.Fatalf("decode: %v", err)
			}
			c.ApplyDefaults()
			if got := c.Search.MaxPageSizeHybridClamp(); got != tc.want {
				t.Errorf("MaxPageSizeHybridClamp() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestPreprocessConfig_FingerprintFormat pins the human-readable format
// (p<ver>-<flags>) so the generation-fingerprint string in stats output
// and DB rows is stable for operators.
func TestPreprocessConfig_FingerprintFormat(t *testing.T) {
	allOn := PreprocessConfig{}
	if got := allOn.Fingerprint(); got != "p1-111111" {
		t.Errorf("default Fingerprint() = %q, want p1-111111", got)
	}

	f := false
	allOff := PreprocessConfig{
		StripQuotes:        &f,
		StripSignatures:    &f,
		StripHTML:          &f,
		StripBase64:        &f,
		StripURLTracking:   &f,
		CollapseWhitespace: &f,
	}
	if got := allOff.Fingerprint(); got != "p1-000000" {
		t.Errorf("all-off Fingerprint() = %q, want p1-000000", got)
	}
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
			if got == baseline {
				t.Errorf("Fingerprint() with %s=false = %q, want different from baseline %q",
					tc.name, got, baseline)
			}
			if other, dup := seen[got]; dup {
				t.Errorf("Fingerprint() with %s=false = %q, collides with %s",
					tc.name, got, other)
			}
			seen[got] = tc.name
		})
	}
}

// TestConfig_GenerationFingerprintFolds checks the full identifier
// composition. The shape is "<model>:<dim>:<preprocess>:c<maxchars>"
// — every segment must contribute so an operator switching the model,
// any preprocess toggle, or the truncation cap stales the existing
// index instead of silently mixing inconsistently-prepared vectors.
func TestConfig_GenerationFingerprintFolds(t *testing.T) {
	base := Config{
		Embeddings: EmbeddingsConfig{Model: "nomic-embed", Dimension: 768, MaxInputChars: 6000},
	}
	got := base.GenerationFingerprint()
	want := "nomic-embed:768:p1-111111:c6000"
	if got != want {
		t.Errorf("GenerationFingerprint() = %q, want %q", got, want)
	}

	// Flipping the model invalidates.
	modelChanged := base
	modelChanged.Embeddings.Model = "snowflake-arctic"
	if modelChanged.GenerationFingerprint() == got {
		t.Error("GenerationFingerprint() did not change when Model changed")
	}

	// Flipping a preprocess toggle invalidates too — this is the gap
	// the reviewer flagged in round one. Without folding Preprocess
	// into the fingerprint, the active generation would keep absorbing
	// new vectors built under a different sanitization policy.
	f := false
	preprocessChanged := base
	preprocessChanged.Preprocess.StripHTML = &f
	if preprocessChanged.GenerationFingerprint() == got {
		t.Error("GenerationFingerprint() did not change when strip_html flipped to false")
	}
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
	if bumped.GenerationFingerprint() == baseline {
		t.Errorf("GenerationFingerprint() did not change when MaxInputChars went 6000→12000 (still %q)",
			baseline)
	}

	// The zero-cap case (Preprocess treats <=0 as "no truncation")
	// must also be distinguishable from any positive cap, otherwise
	// disabling truncation later wouldn't stale a generation that had
	// always been truncating.
	zeroed := base
	zeroed.Embeddings.MaxInputChars = 0
	if zeroed.GenerationFingerprint() == baseline {
		t.Errorf("GenerationFingerprint() did not change when MaxInputChars went 6000→0 (still %q)",
			baseline)
	}
}
