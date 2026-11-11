package api

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
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
		value := jsontext.Value(raw)
		if err := value.Format(jsontext.ReorderRawObjects(true)); err != nil {
			return "", err
		}
		parts = append(parts, string(value))
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("["+strings.Join(parts, ",")+"]"))), nil
}

func apiBaseMarkers(ctx context.Context, base string, explicit bool) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-z", "--name-only", "--full-tree", base, "--", "internal/api/"+apiContractsDir+"/")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && !explicit && ctx.Err() == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("read API contract markers at %q: %w", base, err)
	}
	var markers []string
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
	contracts := make(map[string][]string)
	candidate := make(map[string]bool)
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
			}
			if semver.Compare(v, greatest) > 0 {
				greatest = v
			}
		}
	}
	problem := ""
	for _, marker := range base {
		if !candidate[marker] {
			problem = "restore removed API contract marker " + marker
			break
		}
	}
	if problem == "" && semver.Compare(current, greatest) < 0 {
		problem = fmt.Sprintf("API schema version %s is older than recorded version %s", version, strings.TrimPrefix(greatest, "v"))
	}
	if problem == "" && semver.Compare(current, greatest) > 0 {
		return fmt.Errorf("record the API contract with an empty marker at %s", filepath.Join(apiContractsDir, version+"_"+digest))
	}
	if problem == "" && (len(contracts[current]) != 1 || contracts[current][0] != digest) {
		problem = "API contract changed or has multiple markers for " + version
	}
	if problem != "" {
		major, minor, _ := strings.Cut(strings.TrimPrefix(semver.MajorMinor(greatest), "v"), ".")
		n, _ := new(big.Int).SetString(minor, 10)
		n.Add(n, big.NewInt(1))
		return fmt.Errorf("%s; use %s.%s.0 and move feature minimum constants introduced by your change", problem, major, n)
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
	require.NoError(checkAPIContract(APISchemaVersion, digest, markers, baseMarkers))
}

func TestCheckAPIContract(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, version, digest, want string
		markers, base               []string
	}{
		{"collision", "5.1.0", b, "5.2.0", []string{"5.1.0_" + a, "5.1.0_" + b}, []string{"5.1.0_" + a}},
		{"matching", "5.0.0", a, "", []string{"5.0.0_" + a}, []string{"5.0.0_" + a}},
		{"stale", "5.0.0", b, "5.1.0", []string{"5.0.0_" + a}, nil},
		{"new", "5.1.0", b, "5.1.0_" + b, []string{"5.0.0_" + a}, nil},
		{"older", "4.0.0", b, "5.1.0", []string{"5.0.0_" + a}, nil},
		{"occupied next minor", "5.1.0", b, "5.3.0", []string{"5.1.0_" + a, "5.2.0_" + b}, nil},
		{"removed", "5.1.0", b, "5.2.0", []string{"5.1.0_" + b}, []string{"5.1.0_" + a}},
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
	markers, err = apiBaseMarkers(t.Context(), "HEAD", false)
	require.NoError(err)
	assert.Contains(markers, "5.0.0_f4c3cd16ffd3e2294f6c0690df7ae8b2024db4b66538f467bc2d2cba36b325cf")
}

func TestAPIContractDigest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	base, err := apiContractDigest(OpenAPIDocument(), openAPIClientDocument())
	require.NoError(err)
	public, client := OpenAPIDocument(), openAPIClientDocument()
	public.Info.Version, client.Info.Version = "9.0.0", "9.0.0"
	versionOnly, err := apiContractDigest(public, client)
	require.NoError(err)
	assert.Equal(base, versionOnly)
	public = OpenAPIDocument()
	public.Paths["/api/v1/test-contract"] = &huma.PathItem{Get: &huma.Operation{OperationID: "test-contract"}}
	changed, err := apiContractDigest(public, openAPIClientDocument())
	require.NoError(err)
	assert.NotEqual(base, changed)
}
