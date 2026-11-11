package api

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"fmt"
	"os"
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

func checkAPIContract(version, digest string, markers []string) error {
	current := "v" + version
	if semver.Canonical(current) != current {
		return fmt.Errorf("invalid API schema version %q", version)
	}
	greatest := ""
	contracts := make(map[string][]string)
	for _, marker := range markers {
		v, hash, ok := strings.Cut(marker, "_")
		if !ok || semver.Canonical("v"+v) != "v"+v || len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			return fmt.Errorf("invalid API contract marker %q", marker)
		}
		v = "v" + v
		contracts[v] = append(contracts[v], hash)
		if semver.Compare(v, greatest) > 0 {
			greatest = v
		}
	}
	if semver.Compare(current, greatest) < 0 {
		return fmt.Errorf("API schema version %s is older than recorded version %s", version, strings.TrimPrefix(greatest, "v"))
	}
	if semver.Compare(current, greatest) > 0 {
		return fmt.Errorf("record the API contract with an empty marker at %s", filepath.Join(apiContractsDir, version+"_"+digest))
	}
	if len(contracts[current]) != 1 || contracts[current][0] != digest {
		majorMinor := semver.MajorMinor(greatest)
		major, minor, _ := strings.Cut(strings.TrimPrefix(majorMinor, "v"), ".")
		n, err := strconv.Atoi(minor)
		if err != nil {
			return fmt.Errorf("parse API version %s: %w", greatest, err)
		}
		return fmt.Errorf("API contract changed or has multiple markers for %s; use %s.%d.0 and move feature minimum constants introduced by your change", version, major, n+1)
	}
	return nil
}

func TestAPISchemaVersionContract(t *testing.T) {
	digest, err := apiContractDigest(OpenAPIDocument(), openAPIClientDocument())
	require.NoError(t, err)
	entries, err := os.ReadDir(apiContractsDir)
	require.NoError(t, err)
	markers := make([]string, 0, len(entries))
	for _, entry := range entries {
		markers = append(markers, entry.Name())
	}
	require.NoError(t, checkAPIContract(APISchemaVersion, digest, markers))
}

func TestCheckAPIContract(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, version, digest, want string
		markers                     []string
	}{
		{"matching", "5.0.0", a, "", []string{"5.0.0_" + a}},
		{"collision", "5.1.0", a, "5.2.0", []string{"5.0.0_" + a, "5.1.0_" + a, "5.1.0_" + b}},
		{"stale", "5.0.0", b, "5.1.0", []string{"5.0.0_" + a}},
		{"new", "5.1.0", b, "5.1.0_" + b, []string{"5.0.0_" + a}},
		{"older", "4.0.0", b, "older", []string{"5.0.0_" + a}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAPIContract(tc.version, tc.digest, tc.markers)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestAPIContractDigest(t *testing.T) {
	base, err := apiContractDigest(OpenAPIDocument(), openAPIClientDocument())
	require.NoError(t, err)
	public, client := OpenAPIDocument(), openAPIClientDocument()
	public.Info.Version, client.Info.Version = "9.0.0", "9.0.0"
	versionOnly, err := apiContractDigest(public, client)
	require.NoError(t, err)
	assert.Equal(t, base, versionOnly)
	public = OpenAPIDocument()
	public.Paths["/api/v1/test-contract"] = &huma.PathItem{Get: &huma.Operation{OperationID: "test-contract"}}
	changed, err := apiContractDigest(public, openAPIClientDocument())
	require.NoError(t, err)
	assert.NotEqual(t, base, changed)
}
