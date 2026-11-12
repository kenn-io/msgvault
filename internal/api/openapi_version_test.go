package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func checkAPIContract(version, digest string, markers, base []string) error {
	current := "v" + version
	greatest := ""
	contracts := make(map[string]map[string]bool)
	candidate := make(map[string]bool)
	baseVersions := make(map[string]bool)
	for i, list := range [][]string{markers, base} {
		for _, marker := range list {
			parts := strings.Split(marker, "_")
			if len(parts) != 3 || semver.Canonical("v"+parts[0]) != "v"+parts[0] || len(parts[1]) != 64 || strings.Trim(parts[1], "0123456789abcdef") != "" || len(parts[2]) != 64 || strings.Trim(parts[2], "0123456789abcdef") != "" {
				return fmt.Errorf("invalid API contract marker %q; use <version>_<sha256>_<summary-sha256> in %s", marker, apiContractsDir)
			}
			v, hash := "v"+parts[0], parts[1]
			if i == 0 {
				candidate[parts[0]+"_"+hash] = true
				if contracts[v] == nil {
					contracts[v] = make(map[string]bool)
				}
				contracts[v][hash] = true
			} else {
				baseVersions[v] = true
			}
			if semver.Compare(v, greatest) > 0 {
				greatest = v
			}
		}
	}
	nextMinor := func(problem string) error {
		major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor(greatest), "v"), ".")
		n, _ := strconv.Atoi(minor)
		return fmt.Errorf("%s; use %s.%d.0 and move feature minimum constants introduced by your change", problem, major, n+1)
	}
	if len(contracts[current]) > 1 {
		return nextMinor("API contract has multiple markers for " + version)
	}
	for v, hashes := range contracts {
		if v != current && len(hashes) > 1 {
			return fmt.Errorf("remove the superseded API contract marker for %s; keep the marker recorded on the base", strings.TrimPrefix(v, "v"))
		}
	}
	problem := ""
	for _, marker := range base {
		identity := marker[:strings.LastIndex(marker, "_")]
		if !candidate[identity] {
			return fmt.Errorf("restore removed API contract marker %s", marker)
		}
	}
	if problem == "" && semver.Compare(current, greatest) < 0 {
		problem = fmt.Sprintf("API schema version %s is older than recorded version %s", version, strings.TrimPrefix(greatest, "v"))
	}
	if problem == "" && semver.Compare(current, greatest) > 0 {
		return fmt.Errorf("record the API contract with a marker at %s", filepath.Join(apiContractsDir, version+"_"+digest+"_<summary-sha256>"))
	}
	if problem == "" && (len(contracts[current]) != 1 || !contracts[current][digest]) {
		problem = "API contract changed or has multiple markers for " + version
	}
	if problem != "" {
		if len(contracts[current]) == 1 && base != nil && !baseVersions[current] && semver.Compare(current, greatest) >= 0 {
			return fmt.Errorf("%s; replace your unreleased marker with %s", problem, filepath.Join(apiContractsDir, version+"_"+digest+"_<summary-sha256>"))
		}
		return nextMinor(problem)
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
		hash := sha256.Sum256([]byte(summary))
		assert.True(t, strings.HasSuffix(entry.Name(), "_"+hex.EncodeToString(hash[:])), entry.Name())
	}
}

func TestCheckAPIContract(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, tc := range []struct {
		name, version, digest, want string
		markers, base               []string
	}{
		{"matching contracts with different summaries", "5.1.0", a, "", []string{"5.1.0_" + a + "_" + b, "5.1.0_" + a + "_" + c}, []string{"5.1.0_" + a + "_" + b}},
		{"corrected summary", "5.1.0", a, "", []string{"5.1.0_" + a + "_" + c}, []string{"5.1.0_" + a + "_" + b}},
		{"combined collision", "5.1.0", c, "5.2.0", []string{"5.0.0_" + a, "5.1.0_" + a, "5.1.0_" + b}, []string{"5.0.0_" + a}},
		{"combined collision reversed", "5.1.0", c, "5.2.0", []string{"5.0.0_" + a, "5.1.0_" + b, "5.1.0_" + a}, []string{"5.0.0_" + a}},
		{"combined collision occupied", "5.1.0", c, "5.3.0", []string{"5.0.0_" + a, "5.1.0_" + a, "5.1.0_" + b, "5.2.0_" + a}, []string{"5.0.0_" + a}},
		{"collision repaired", "5.2.0", c, "", []string{"5.0.0_" + a, "5.1.0_" + a, "5.2.0_" + c}, []string{"5.0.0_" + a, "5.1.0_" + a}},
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
			for _, list := range [][]string{tc.markers, tc.base} {
				for i, marker := range list {
					if strings.Count(marker, "_") == 1 {
						list[i] += "_" + c
					}
				}
			}
			err := checkAPIContract(tc.version, tc.digest, tc.markers, tc.base)
			if tc.want == "" {
				require.NoError(err)
			} else {
				require.ErrorContains(err, tc.want)
			}
		})
	}
}

func TestMatchingContractSummaries(t *testing.T) {
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "config"))
	t.Chdir(t.TempDir())
	git := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q", "-b", "base")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "base", "--allow-empty")

	digest := strings.Repeat("a", 64)
	var markers []string
	summaries := []string{"First summary", "Second summary"}
	for i, summary := range summaries {
		git("switch", "-q", "-c", fmt.Sprintf("change-%d", i), "base")
		require.NoError(t, os.MkdirAll(apiContractsDir, 0o755))
		hash := sha256.Sum256([]byte(summary))
		name := fmt.Sprintf("5.1.0_%s_%x", digest, hash)
		require.NoError(t, os.WriteFile(filepath.Join(apiContractsDir, name), []byte(summary+"\n"), 0o644))
		git("add", apiContractsDir)
		git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "change")
		markers = append(markers, name)
	}
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "merge", "-q", "change-0")
	entries, err := os.ReadDir(apiContractsDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.NotEqual(t, markers[0], markers[1])
	for i, marker := range markers {
		content, err := os.ReadFile(filepath.Join(apiContractsDir, marker))
		require.NoError(t, err)
		assert.Equal(t, summaries[i]+"\n", string(content))
	}
	require.NoError(t, checkAPIContract("5.1.0", digest, markers, markers[:1]))
}

func TestAPIBaseMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	_, err := apiBaseMarkers(t.Context(), "refs/heads/nonexistent-api-contract-base")
	require.ErrorContains(err, "read API contract markers")
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
	markers, err := apiBaseMarkers(t.Context(), "HEAD")
	require.NoError(err)
	assert.Equal([]string{marker}, markers)
}

func TestAPIContractDigest(t *testing.T) {
	makeDoc := func(doc *huma.OpenAPI) *huma.OpenAPI {
		doc.Paths["/api/v1/contract-test"] = &huma.PathItem{Post: &huma.Operation{
			OperationID: "contract-test", Summary: "original", Description: "original",
			Parameters:  []*huma.Param{{Name: "query", In: "query", Description: "original"}},
			RequestBody: &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"description": {Type: "string"}, "summary": {Type: "string"}}}}}},
			Responses:   map[string]*huma.Response{"200": {Description: "original", Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{Type: "string"}}}}},
		}}
		return doc
	}
	base, err := apiContractDigest(makeDoc(OpenAPIDocument()), makeDoc(openAPIClientDocument()))
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
		{"webhook named description", func(d *huma.OpenAPI) {
			d.Webhooks = map[string]*huma.PathItem{"description": {Post: &huma.Operation{OperationID: "hook"}}}
		}, false},
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
		for _, document := range []string{"public", "client"} {
			t.Run(tc.name+"/"+document, func(t *testing.T) {
				public := makeDoc(OpenAPIDocument())
				client := makeDoc(openAPIClientDocument())
				if document == "public" {
					tc.change(public)
				} else {
					tc.change(client)
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
}
