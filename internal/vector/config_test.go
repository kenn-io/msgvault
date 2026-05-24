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
