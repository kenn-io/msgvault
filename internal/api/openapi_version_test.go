package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

func apiContractDigest(docs ...*huma.OpenAPI) (string, error) {
	parts := make([]string, 0, len(docs))
	for _, doc := range docs {
		doc.Info.Version = ""
		raw, err := doc.MarshalJSON()
		if err != nil {
			return "", fmt.Errorf("marshal API document: %w", err)
		}
		var document any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			return "", err
		}
		normalizeAPIContract(document, false)
		raw, err = json.Marshal(document)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(raw))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("["+strings.Join(parts, ",")+"]"))), nil
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
			case "description", "summary", "title", "example", "examples", "externalDocs", "patternDescription":
				delete(v, key)
			case "default", "enum", "const", "dependentRequired", "security", "mapping":
				continue
			default:
				if strings.HasPrefix(key, "x-") {
					continue
				}
				switch key {
				case "paths", "schemas", "properties", "patternProperties", "responses", "content", "headers", "parameters", "requestBodies", "securitySchemes", "callbacks", "links", "encoding", "variables", "webhooks":
					normalizeAPIContract(item, true)
				default:
					normalizeAPIContract(item, false)
				}
			}
		}
	}
}

func apiBaseMarkers(ctx context.Context, base string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-z", "--name-only", "--full-tree", base, "--", "internal/api/"+apiContractsDir+"/")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read API contract markers at %q: %w", base, err)
	}
	markers := []string{}
	for path := range strings.SplitSeq(string(out), "\x00") {
		if path != "" {
			markers = append(markers, strings.TrimPrefix(path, "internal/api/"+apiContractsDir+"/"))
		}
	}
	return markers, nil
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
			return fmt.Errorf("restore removed API contract marker %s", marker)
		}
	}
	marker := filepath.Join(apiContractsDir, version+"_"+digest+"_<summary-sha256>")
	switch c := semver.Compare(current, greatest); {
	case c < 0:
		return nextMinor(fmt.Sprintf("API schema version %s is older than recorded version %s", version, strings.TrimPrefix(greatest, "v")))
	case c > 0:
		return fmt.Errorf("record the API contract with a marker at %s", marker)
	case !contracts[current][digest]:
		return fmt.Errorf("API contract changed under %s; if %s is unreleased, replace your marker with %s; otherwise compare with MSGVAULT_API_CONTRACT_BASE or use %s and move feature minimum constants introduced by your change", version, version, marker, next)
	}
	return nil
}

func TestAPISchemaVersionContract(t *testing.T) {
	require := require.New(t)
	digest, err := apiContractDigest(OpenAPIDocument(), openAPIClientDocument())
	require.NoError(err)
	entries, err := os.ReadDir(apiContractsDir)
	require.NoError(err)
	markers := make([]string, 0, len(entries))
	for _, entry := range entries {
		markers = append(markers, entry.Name())
	}
	var baseMarkers []string
	if base, ok := os.LookupEnv("MSGVAULT_API_CONTRACT_BASE"); ok {
		baseMarkers, err = apiBaseMarkers(t.Context(), base)
		require.NoError(err)
	} else {
		t.Log("MSGVAULT_API_CONTRACT_BASE is unset; skipping history comparison")
	}
	require.NoError(checkAPIContract(APISchemaVersion, digest, markers, baseMarkers))
}

func TestAPIVersionMarkerSummary(t *testing.T) {
	entries, err := os.ReadDir(apiContractsDir)
	require.NoError(t, err)
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(apiContractsDir, entry.Name()))
		require.NoError(t, err)
		summary := strings.TrimSuffix(strings.TrimSuffix(string(content), "\n"), "\r")
		assert.NotEmpty(t, strings.TrimSpace(summary), entry.Name())
		assert.NotContains(t, summary, "\n", entry.Name())
		assert.NotContains(t, summary, "\r", entry.Name())
	}
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
		{"collision", "5.1.0", b, "use 5.2.0", []string{m("5.0.0", a), m("5.1.0", a), m("5.1.0", b)}, []string{m("5.0.0", a)}},
		{"collision with next minor taken", "5.1.0", b, "use 5.3.0", []string{m("5.0.0", a), m("5.1.0", a), m("5.1.0", b), m("5.2.0", a)}, []string{m("5.0.0", a)}},
		{"collision repaired", "5.2.0", b, "", []string{m("5.0.0", a), m("5.1.0", a), m("5.2.0", b)}, []string{m("5.0.0", a), m("5.1.0", a)}},
		{"edited marker", "5.1.0", b, "replace your marker with testdata/api-version-contracts/5.1.0_" + b + "_<summary-sha256>; otherwise compare with MSGVAULT_API_CONTRACT_BASE or use 5.2.0", []string{m("5.0.0", a), m("5.1.0", a)}, nil},
		{"superseded", "5.2.0", b, "remove the superseded", []string{m("5.1.0", a), m("5.1.0", b), m("5.2.0", b)}, []string{m("5.1.0", a)}},
		{"unrecorded", "5.1.0", b, "marker at", []string{m("5.0.0", a)}, nil},
		{"older", "4.0.0", b, "older than recorded version", []string{m("5.0.0", a)}, nil},
		{"malformed", "5.0.0", a, "<version>_<sha256>", []string{"bad"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAPIContract(tc.version, tc.digest, tc.markers, tc.base)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestAPIBaseMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	marker := "5.0.0_" + strings.Repeat("f", 64) + "_" + strings.Repeat("a", 64)
	require.NoError(os.MkdirAll("internal/api/"+apiContractsDir, 0o755))
	require.NoError(os.WriteFile("internal/api/"+apiContractsDir+"/"+marker, nil, 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"}} {
		require.NoError(exec.CommandContext(t.Context(), "git", args...).Run())
	}
	_, err := apiBaseMarkers(t.Context(), "refs/heads/nonexistent-api-contract-base")
	require.ErrorContains(err, "read API contract markers")
	markers, err := apiBaseMarkers(t.Context(), "HEAD")
	require.NoError(err)
	assert.Equal([]string{marker}, markers)
}

func TestAPIContractDigest(t *testing.T) {
	makeDoc := func(doc *huma.OpenAPI) *huma.OpenAPI {
		doc.Webhooks = map[string]*huma.PathItem{"description": {Post: &huma.Operation{OperationID: "hook"}}}
		doc.Paths["/api/v1/contract-test"] = &huma.PathItem{Post: &huma.Operation{
			OperationID: "contract-test", Summary: "original", Description: "original",
			Parameters:  []*huma.Param{{Name: "query", In: "query", Description: "original"}},
			RequestBody: &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"description": {Type: "string"}}}}}},
			Responses:   map[string]*huma.Response{"200": {Description: "original", Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "string"}}}}},
		}}
		return doc
	}
	base, err := apiContractDigest(makeDoc(OpenAPIDocument()), makeDoc(openAPIClientDocument()))
	require.NoError(t, err)
	op := func(d *huma.OpenAPI) *huma.Operation { return d.Paths["/api/v1/contract-test"].Post }
	for _, tc := range []struct {
		name   string
		change func(*huma.OpenAPI)
		same   bool
		client bool
	}{
		{"wording", func(d *huma.OpenAPI) {
			op(d).Summary = "new"
			op(d).Description = "new"
			op(d).Parameters[0].Description = "new"
			op(d).Responses["200"].Description = "new"
			op(d).RequestBody.Content["application/json"].Schema.Examples = []any{"example"}
		}, true, false},
		{"version", func(d *huma.OpenAPI) { d.Info.Version = "9.0.0" }, true, false},
		{"client request", func(d *huma.OpenAPI) { op(d).RequestBody.Required = true }, false, true},
		{"route", func(d *huma.OpenAPI) {
			d.Paths["/api/v1/another"] = &huma.PathItem{Get: &huma.Operation{OperationID: "another"}}
		}, false, false},
		{"parameter", func(d *huma.OpenAPI) { op(d).Parameters[0].Required = true }, false, false},
		{"response", func(d *huma.OpenAPI) { op(d).Responses["200"].Content["application/json"].Schema.Type = "integer" }, false, false},
		{"webhook named description", func(d *huma.OpenAPI) { d.Webhooks["description"].Post.OperationID = "changed-hook" }, false, false},
		{"property named description", func(d *huma.OpenAPI) {
			op(d).RequestBody.Content["application/json"].Schema.Properties["description"].Type = "integer"
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			public, client := makeDoc(OpenAPIDocument()), makeDoc(openAPIClientDocument())
			if tc.client {
				tc.change(client)
			} else {
				tc.change(public)
			}
			digest, err := apiContractDigest(public, client)
			require.NoError(t, err)
			if tc.same {
				assert.Equal(t, base, digest)
			} else {
				assert.NotEqual(t, base, digest)
			}
		})
	}
}
