package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

const apiContractsDir = "testdata/api-version-contracts"

func apiContractDigest(t *testing.T, docs ...*huma.OpenAPI) string {
	t.Helper()
	parts := make([]string, 0, len(docs))
	for _, doc := range docs {
		doc.Info.Version = ""
		raw, err := doc.MarshalJSON()
		require.NoError(t, err)
		var document any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&document))
		normalizeAPIContract(document, false)
		raw, err = json.Marshal(document)
		require.NoError(t, err)
		parts = append(parts, string(raw))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("["+strings.Join(parts, ",")+"]")))
}

// Map keys in OpenAPI containers are user-defined names, not field names.
func normalizeAPIContract(value any, namedMap bool) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			normalizeAPIContract(item, false)
		}
	case map[string]any:
		for key, item := range v {
			if namedMap {
				normalizeAPIContract(item, false)
				continue
			}
			switch key {
			case "description", "summary", "title", "example", "examples", "externalDocs", "patternDescription", "tags":
				delete(v, key)
			case "default", "enum", "const", "dependentRequired", "security", "mapping":
				continue
			case "paths", "schemas", "properties", "patternProperties", "responses", "content", "headers", "parameters", "requestBodies", "securitySchemes", "callbacks", "links", "encoding", "variables", "webhooks":
				normalizeAPIContract(item, true)
			default:
				if strings.HasPrefix(key, "x-") {
					continue
				}
				normalizeAPIContract(item, false)
			}
		}
	}
}

func apiBaseMarkers(t *testing.T, base string) []string {
	t.Helper()
	if base == "" {
		out, err := exec.CommandContext(t.Context(), "git", "merge-base", "HEAD", "origin/main").Output()
		if err != nil && os.Getenv("CI") == "" {
			t.Skip("API contract base unavailable; fetch origin/main or set MSGVAULT_API_CONTRACT_BASE")
		}
		require.NoError(t, err, "find API contract base; fetch origin/main and its history or set MSGVAULT_API_CONTRACT_BASE to a base SHA")
		base = strings.TrimSpace(string(out))
	}
	cmd := exec.CommandContext(t.Context(), "git", "ls-tree", "-z", "--name-only", "--full-tree", base, "--", "internal/api/"+apiContractsDir+"/") // #nosec G702 -- base is a Git ref supplied by the test runner.
	out, err := cmd.Output()
	if err != nil && os.Getenv("CI") == "" {
		t.Skipf("API contract base %q unavailable; fetch it or set MSGVAULT_API_CONTRACT_BASE", base)
	}
	require.NoError(t, err, "read API contract markers at %q", base)
	markers := []string{}
	for path := range strings.SplitSeq(string(out), "\x00") {
		if path != "" {
			markers = append(markers, strings.TrimPrefix(path, "internal/api/"+apiContractsDir+"/"))
		}
	}
	return markers
}

var apiMarkerPattern = regexp.MustCompile(`^(\d+\.\d+\.\d+)_([0-9a-f]{64})_([0-9a-f]{64})$`)

// parseAPIMarker returns "v<version>" and the contract digest of a marker name.
func parseAPIMarker(name string) (string, string, bool) {
	m := apiMarkerPattern.FindStringSubmatch(name)
	if m == nil || semver.Canonical("v"+m[1]) != "v"+m[1] {
		return "", "", false
	}
	return "v" + m[1], m[2], true
}

func checkAPIMarkerSummary(name string, content []byte) error {
	summary, crlf := strings.CutSuffix(string(content), "\r\n")
	if !crlf {
		summary = strings.TrimSuffix(summary, "\n")
	}
	if strings.TrimSpace(summary) == "" || strings.ContainsAny(summary, "\r\n") {
		return fmt.Errorf("API contract marker %q needs a nonempty one-line summary", name)
	}
	return nil
}

func checkAPIContract(version, digest string, markers, base []string) error {
	current := "v" + version
	greatest := ""
	contracts := make(map[string]map[string]bool)
	candidates := make(map[string]bool)
	for i, list := range [][]string{markers, base} {
		for _, marker := range list {
			v, hash, ok := parseAPIMarker(marker)
			if !ok {
				return fmt.Errorf("invalid API contract marker %q; use <version>_<sha256>_<summary-sha256> in %s", marker, apiContractsDir)
			}
			if i == 0 {
				candidates[marker] = true
				if contracts[v] == nil {
					contracts[v] = make(map[string]bool)
				}
				contracts[v][hash] = true
			}
			if semver.Compare(v, greatest) > 0 {
				greatest = v
			}
		}
	}
	marker := path.Join(apiContractsDir, version+"_"+digest+"_<summary-sha256>")
	major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor(greatest), "v"), ".")
	n, _ := strconv.Atoi(minor)
	next := fmt.Sprintf("%s.%d.0", major, n+1)
	nextMinor := func(problem string) error {
		return fmt.Errorf("%s; use %s and move feature minimum constants introduced by your change", problem, next)
	}
	if len(contracts[current]) > 1 {
		return nextMinor("API contract has multiple markers for " + version)
	}
	for v, hashes := range contracts {
		if v != current && len(hashes) > 1 {
			return fmt.Errorf("remove the superseded API contract marker for %s; keep the marker recorded on the base", strings.TrimPrefix(v, "v"))
		}
	}
	for _, marker := range base {
		if !candidates[marker] {
			return nextMinor("restore removed API contract marker " + marker)
		}
	}
	switch c := semver.Compare(current, greatest); {
	case c < 0:
		return nextMinor("API schema version " + version + " is older than " + strings.TrimPrefix(greatest, "v"))
	case c > 0:
		return fmt.Errorf("record the API contract with a marker at %s", marker)
	case !contracts[current][digest]:
		return fmt.Errorf("API contract changed under %s; if %s is unreleased, replace your marker with %s; otherwise compare with MSGVAULT_API_CONTRACT_BASE or use %s and move feature minimum constants introduced by your change", version, version, marker, next)
	}
	return nil
}

func TestAPISchemaVersionContract(t *testing.T) {
	require := require.New(t)
	digest := apiContractDigest(t, OpenAPIDocument(), openAPIClientDocument())
	entries, err := os.ReadDir(apiContractsDir)
	require.NoError(err)
	markers := make([]string, 0, len(entries))
	for _, entry := range entries {
		markers = append(markers, entry.Name())
		content, err := os.ReadFile(filepath.Join(apiContractsDir, entry.Name()))
		require.NoError(err)
		require.NoError(checkAPIMarkerSummary(entry.Name(), content))
	}
	baseMarkers := apiBaseMarkers(t, os.Getenv("MSGVAULT_API_CONTRACT_BASE"))
	assert.NoError(t, checkAPIContract(APISchemaVersion, digest, markers, baseMarkers))
}

func TestCheckAPIContract(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	m := func(version, digest string) string { return version + "_" + digest + "_" + strings.Repeat("c", 64) }
	for _, tc := range []struct {
		name, version, digest, want string
		markers, base               []string
	}{
		{"matching contracts with different summaries", "5.1.0", a, "", []string{"5.1.0_" + a + "_" + strings.Repeat("c", 64), "5.1.0_" + a + "_" + strings.Repeat("d", 64)}, []string{"5.1.0_" + a + "_" + strings.Repeat("c", 64)}},
		{"replaced base summary path", "5.1.0", a, "restore removed API contract marker", []string{"5.1.0_" + a + "_" + strings.Repeat("d", 64)}, []string{"5.1.0_" + a + "_" + strings.Repeat("c", 64)}},
		{"collision with next minor taken", "5.1.0", b, "use 5.3.0", []string{m("5.0.0", a), m("5.1.0", a), m("5.1.0", b), m("5.2.0", a)}, []string{m("5.0.0", a), m("5.1.0", a), m("5.2.0", a)}},
		{"collision repaired", "5.2.0", b, "", []string{m("5.0.0", a), m("5.1.0", a), m("5.2.0", b)}, []string{m("5.0.0", a), m("5.1.0", a)}},
		{"rollback", "5.0.0", a, "use 5.2.0", []string{m("5.0.0", a), m("5.1.0", b)}, []string{m("5.0.0", a), m("5.1.0", b)}},
		{"changed base contract", "5.1.0", b, "replace your marker with testdata/api-version-contracts/5.1.0_" + b + "_<summary-sha256>; otherwise compare with MSGVAULT_API_CONTRACT_BASE or use 5.2.0", []string{m("5.0.0", a), m("5.1.0", a)}, []string{m("5.0.0", a), m("5.1.0", a)}},
		{"superseded", "5.2.0", b, "remove the superseded", []string{m("5.1.0", a), m("5.1.0", b), m("5.2.0", b)}, []string{m("5.1.0", a)}},
		{"unrecorded", "5.1.0", b, "marker at testdata/api-version-contracts/5.1.0_" + b + "_<summary-sha256>", []string{m("5.0.0", a)}, []string{m("5.0.0", a)}},
		{"malformed", "5.0.0", a, "<version>_<sha256>", []string{"bad"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAPIContract(tc.version, tc.digest, tc.markers, tc.base)
			if tc.want == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestAPIBaseMarkers(t *testing.T) {
	require := require.New(t)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			require.NoError(os.Unsetenv(key))
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "config"))
	t.Chdir(t.TempDir())
	git := func(args ...string) {
		require.NoError(exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=Test", "-c", "user.email=fixture"}, args...)...).Run())
	}
	dir := "internal/api/" + apiContractsDir + "/"
	marker := "5.0.0_" + strings.Repeat("f", 64) + "_" + strings.Repeat("a", 64)
	require.NoError(os.MkdirAll(dir, 0o755))
	require.NoError(os.WriteFile(dir+marker, []byte("Initial contract\n"), 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "fixture"}} {
		git(args...)
	}
	git("update-ref", "refs/remotes/upstream/main", "HEAD")
	require.NoError(os.Remove(dir + marker))
	assert.Equal(t, []string{marker}, apiBaseMarkers(t, "refs/remotes/upstream/main"))
	t.Setenv("CI", "")
	run := false
	assert.True(t, t.Run("missing base", func(t *testing.T) {
		apiBaseMarkers(t, "")
		run = true
	}))
	assert.False(t, run)
}

func TestAPIMarkerSummary(t *testing.T) {
	name := "5.0.0_" + strings.Repeat("a", 64) + "_" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"valid", "change\n", true},
		{"empty", "", false},
		{"multiline", "change\nanother\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.valid, checkAPIMarkerSummary(name, []byte(tc.content)) == nil)
		})
	}
}

func TestAPIContractDigest(t *testing.T) {
	makeDoc := func() *huma.OpenAPI {
		doc := &huma.OpenAPI{Info: &huma.Info{}, Paths: map[string]*huma.PathItem{}}
		doc.Paths["/api/v1/contract-test"] = &huma.PathItem{Post: &huma.Operation{
			OperationID: "contract-test", Summary: "original", Description: "original", Tags: []string{"original"},
			Parameters:  []*huma.Param{{Name: "query", In: "query"}},
			RequestBody: &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"description": {Type: "string"}}}}}},
			Responses:   map[string]*huma.Response{"200": {Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "string"}}}}},
		}}
		schema := doc.Paths["/api/v1/contract-test"].Post.RequestBody.Content["application/json"].Schema
		schema.Default = map[string]any{"description": "original"}
		schema.Extensions = map[string]any{"x-contract": map[string]any{"description": "original"}}
		return doc
	}
	base := apiContractDigest(t, makeDoc(), makeDoc())
	op := func(d *huma.OpenAPI) *huma.Operation { return d.Paths["/api/v1/contract-test"].Post }
	for _, tc := range []struct {
		name   string
		change func(public, client *huma.OpenAPI)
		same   bool
	}{
		{"wording", func(d, _ *huma.OpenAPI) {
			op(d).Summary = "new"
			op(d).Description = "new"
			op(d).RequestBody.Content["application/json"].Schema.Examples = []any{"example"}
		}, true},
		{"version", func(d, _ *huma.OpenAPI) { d.Info.Version = "9.0.0" }, true},
		{"tag", func(d, _ *huma.OpenAPI) { op(d).Tags = []string{"renamed"} }, true},
		{"client request", func(_, d *huma.OpenAPI) { op(d).RequestBody.Required = true }, false},
		{"route", func(d, _ *huma.OpenAPI) {
			d.Paths["/api/v1/another"] = &huma.PathItem{Get: &huma.Operation{OperationID: "another"}}
		}, false},
		{"parameter", func(d, _ *huma.OpenAPI) { op(d).Parameters[0].Required = true }, false},
		{"response", func(d, _ *huma.OpenAPI) { op(d).Responses["200"].Content["application/json"].Schema.Type = "integer" }, false},
		{"object default", func(d, _ *huma.OpenAPI) {
			op(d).RequestBody.Content["application/json"].Schema.Default = map[string]any{"description": "changed"}
		}, false},
		{"nested client extension", func(_, d *huma.OpenAPI) {
			op(d).RequestBody.Content["application/json"].Schema.Extensions["x-contract"] = map[string]any{"description": "changed"}
		}, false},
		{"property named description", func(d, _ *huma.OpenAPI) {
			op(d).RequestBody.Content["application/json"].Schema.Properties["description"].Type = "integer"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public, client := makeDoc(), makeDoc()
			tc.change(public, client)
			digest := apiContractDigest(t, public, client)
			assert.Equal(t, tc.same, digest == base)
		})
	}
}
