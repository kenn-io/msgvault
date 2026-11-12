package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		value := jsontext.Value(raw)
		if err := value.Format(jsontext.ReorderRawObjects(true)); err != nil {
			return "", err
		}
		parts = append(parts, string(value))
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
				case "paths", "schemas", "properties", "patternProperties", "responses", "content", "headers", "parameters", "requestBodies", "securitySchemes", "callbacks", "links", "encoding", "mapping", "variables":
					normalizeAPIContract(item, true)
				default:
					normalizeAPIContract(item, false)
				}
			}
		}
	}
}

func apiBaseMarkers(ctx context.Context, base string, explicit bool) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-z", "--name-only", "--full-tree", base, "--", "internal/api/"+apiContractsDir+"/")
	out, err := cmd.Output()
	if err != nil {
		if !explicit {
			return nil, nil
		}
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

func checkAPIContract(version, digest string, markers, base []string) error {
	current := "v" + version
	greatest := ""
	baseGreatest := ""
	contracts := make(map[string][]string)
	candidate := make(map[string]bool)
	baseVersions := make(map[string]bool)
	for i, list := range [][]string{markers, base} {
		for _, marker := range list {
			v, hash, ok := strings.Cut(marker, "_")
			if !ok || semver.Canonical("v"+v) != "v"+v || len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
				return fmt.Errorf("invalid API contract marker %q; use <version>_<sha256> in %s", marker, apiContractsDir)
			}
			v = "v" + v
			if i == 0 {
				candidate[marker] = true
				contracts[v] = append(contracts[v], hash)
			} else {
				baseVersions[v] = true
				if semver.Compare(v, baseGreatest) > 0 {
					baseGreatest = v
				}
			}
			if semver.Compare(v, greatest) > 0 {
				greatest = v
			}
		}
	}
	for v, hashes := range contracts {
		if v != current && len(hashes) > 1 {
			return fmt.Errorf("remove the superseded API contract marker for %s; keep the marker recorded on the base", strings.TrimPrefix(v, "v"))
		}
	}
	problem := ""
	for _, marker := range base {
		if !candidate[marker] {
			return fmt.Errorf("restore removed API contract marker %s", marker)
		}
	}
	if problem == "" && semver.Compare(current, greatest) < 0 {
		problem = fmt.Sprintf("API schema version %s is older than recorded version %s", version, strings.TrimPrefix(greatest, "v"))
	}
	if problem == "" && semver.Compare(current, greatest) > 0 {
		return fmt.Errorf("record the API contract with a marker at %s", filepath.Join(apiContractsDir, version+"_"+digest))
	}
	if problem == "" && (len(contracts[current]) != 1 || contracts[current][0] != digest) {
		problem = "API contract changed or has multiple markers for " + version
	}
	if problem != "" {
		if base != nil && !baseVersions[current] && semver.Compare(current, greatest) >= 0 {
			return fmt.Errorf("%s; replace your unreleased marker with %s", problem, filepath.Join(apiContractsDir, version+"_"+digest))
		}
		if baseGreatest != "" {
			greatest = baseGreatest
		}
		major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor(greatest), "v"), ".")
		n, _ := strconv.Atoi(minor)
		return fmt.Errorf("%s; use %s.%d.0 and move feature minimum constants introduced by your change", problem, major, n+1)
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
	base, explicit := os.LookupEnv("MSGVAULT_API_CONTRACT_BASE")
	if !explicit {
		base = "origin/main"
	}
	baseMarkers, err := apiBaseMarkers(t.Context(), base, explicit)
	require.NoError(err)
	if baseMarkers == nil {
		t.Log("API contract base unavailable; skipping history comparison")
	}
	require.NoError(checkAPIContract(APISchemaVersion, digest, markers, baseMarkers))
}

func TestAPIVersionMarkerSummary(t *testing.T) {
	entries, err := os.ReadDir(apiContractsDir)
	require.NoError(t, err)
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(apiContractsDir, entry.Name()))
		require.NoError(t, err)
		assert.NotEmpty(t, strings.TrimSpace(string(content)), entry.Name())
	}
}

func TestCheckAPIContract(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, version, digest, want string
		markers, base               []string
	}{
		{"collision", "5.1.0", a, "5.2.0", []string{"5.1.0_" + a, "5.1.0_" + b}, []string{"5.1.0_" + b}},
		{"unreleased edit", "5.1.0", b, "replace your unreleased marker", []string{"5.0.0_" + a, "5.1.0_" + a}, []string{"5.0.0_" + a}},
		{"superseded", "5.2.0", b, "remove the superseded", []string{"5.1.0_" + a, "5.1.0_" + b, "5.2.0_" + b}, []string{"5.1.0_" + a}},
		{"matching", "5.0.0", a, "", []string{"5.0.0_" + a}, []string{"5.0.0_" + a}},
		{"stale", "5.0.0", b, "5.1.0", []string{"5.0.0_" + a}, nil},
		{"new", "5.1.0", b, "5.1.0_" + b, []string{"5.0.0_" + a}, nil},
		{"older", "4.0.0", b, "older than recorded version", []string{"5.0.0_" + a}, nil},
		{"occupied", "5.1.0", b, "5.3.0", []string{"5.1.0_" + a, "5.1.0_" + b, "5.2.0_" + a}, []string{"5.1.0_" + a, "5.2.0_" + a}},
		{"removed", "5.1.0", b, "restore removed API contract marker", []string{"5.1.0_" + b}, []string{"5.1.0_" + a}},
		{"malformed", "5.0.0", a, "<version>_<sha256>", []string{"bad"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			err := checkAPIContract(tc.version, tc.digest, tc.markers, tc.base)
			if tc.want == "" {
				require.NoError(err)
			} else {
				require.ErrorContains(err, tc.want)
			}
		})
	}
}

func TestAPIBaseMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	_, err := apiBaseMarkers(t.Context(), "refs/heads/nonexistent-api-contract-base", true)
	require.ErrorContains(err, "read API contract markers")
	markers, err := apiBaseMarkers(t.Context(), "refs/heads/nonexistent-api-contract-base", false)
	require.NoError(err)
	assert.Empty(markers)
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
	marker := "5.0.0_f4c3cd16ffd3e2294f6c0690df7ae8b2024db4b66538f467bc2d2cba36b325cf"
	require.NoError(os.MkdirAll("internal/api/"+apiContractsDir, 0o755))
	require.NoError(os.WriteFile("internal/api/"+apiContractsDir+"/"+marker, nil, 0o644))
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture"}} {
		require.NoError(exec.CommandContext(t.Context(), "git", args...).Run())
	}
	markers, err = apiBaseMarkers(t.Context(), "HEAD", true)
	require.NoError(err)
	assert.Equal([]string{marker}, markers)
}

func TestAPIContractDigest(t *testing.T) {
	makeDoc := func() *huma.OpenAPI {
		doc := OpenAPIDocument()
		doc.Paths["/api/v1/contract-test"] = &huma.PathItem{Post: &huma.Operation{
			OperationID: "contract-test", Summary: "original", Description: "original",
			Parameters:  []*huma.Param{{Name: "query", In: "query", Description: "original"}},
			RequestBody: &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"description": {Type: "string"}, "summary": {Type: "string"}}}}}},
			Responses:   map[string]*huma.Response{"200": {Description: "original", Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "string"}}}}},
		}}
		return doc
	}
	base, err := apiContractDigest(makeDoc(), openAPIClientDocument())
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func(*huma.OpenAPI)
		same   bool
	}{
		{"wording", func(d *huma.OpenAPI) {
			op := d.Paths["/api/v1/contract-test"].Post
			op.Summary = "new"
			op.Description = "new"
			op.Parameters[0].Description = "new"
			op.Responses["200"].Description = "new"
			op.RequestBody.Content["application/json"].Schema.Title = "new"
			op.RequestBody.Content["application/json"].Schema.Examples = []any{"example"}
		}, true},
		{"route", func(d *huma.OpenAPI) {
			d.Paths["/api/v1/another"] = &huma.PathItem{Get: &huma.Operation{OperationID: "another"}}
		}, false},
		{"parameter", func(d *huma.OpenAPI) { d.Paths["/api/v1/contract-test"].Post.Parameters[0].Required = true }, false},
		{"request", func(d *huma.OpenAPI) { d.Paths["/api/v1/contract-test"].Post.RequestBody.Required = true }, false},
		{"response", func(d *huma.OpenAPI) {
			d.Paths["/api/v1/contract-test"].Post.Responses["200"].Content["application/json"].Schema.Type = "integer"
		}, false},
		{"tag", func(d *huma.OpenAPI) { d.Paths["/api/v1/contract-test"].Post.Tags = []string{"new"} }, false},
		{"description property", func(d *huma.OpenAPI) {
			d.Paths["/api/v1/contract-test"].Post.RequestBody.Content["application/json"].Schema.Properties["description"].Type = "integer"
		}, false},
		{"summary property", func(d *huma.OpenAPI) {
			d.Paths["/api/v1/contract-test"].Post.RequestBody.Content["application/json"].Schema.Properties["summary"].Type = "integer"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := makeDoc()
			tc.change(doc)
			digest, err := apiContractDigest(doc, openAPIClientDocument())
			require.NoError(t, err)
			if tc.same {
				assert.Equal(t, base, digest)
			} else {
				assert.NotEqual(t, base, digest)
			}
		})
	}
	client := openAPIClientDocument()
	client.Paths["/api/v1/test-contract"] = &huma.PathItem{Get: &huma.Operation{OperationID: "test-contract"}}
	digest, err := apiContractDigest(makeDoc(), client)
	require.NoError(t, err)
	assert.NotEqual(t, base, digest)
}
