# Organization Profile Write Limits Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound organization profile replacement by aggregate value count and expanded inline-media bytes while preserving normal retained-media updates.

**Architecture:** Validate the complete input once at the top of the store preparation path, before collection-specific work. Preflight retained media from active-row metadata before loading blobs, then load each distinct content hash once. Surface the typed size error as HTTP 413 and regenerate every committed API artifact.

**Tech Stack:** Go, SQLite/PostgreSQL store abstraction, `net/http`, Huma OpenAPI generation, testify, Bun web type generation.

## Global Constraints

- Accept at most 200 total values across names, identifiers, addresses, contact points, media, and categories.
- Keep the existing 8 MiB limit for each inline media value.
- Accept at most 32 MiB of logical inline media across the desired active organization profile.
- Count retained bytes once per desired media row, even when rows share a content hash.
- Enforce limits strictly for profiles created before this change.
- Validate limits only in the store preparation path; do not duplicate validation in the HTTP converter.
- Check aggregate value count before any per-row normalization or service lookup.
- Preflight retained bytes from `OrganizationMedia.ByteSize` before loading any blob.
- Use testify assertions and run Go tests with `-tags "fts5 sqlite_vec"`.
- Keep all public branch, commit, and pull-request wording focused on the correctness behavior.

## File Map

- `internal/store/organization_profile.go`: limit constants, typed error, input preflight, retained-media budget, and per-hash load cache.
- `internal/store/organization_profile_test.go`: store-boundary and transaction-rollback regressions.
- `internal/api/organizations.go`: HTTP 413 mapping and organization profile operation contract.
- `internal/api/organizations_test.go`: end-to-end HTTP limit response.
- `internal/api/openapi_test.go`: generated-operation response contract.
- `api/openapi.yaml`: generated OpenAPI 3.1 contract.
- `pkg/client/openapi.yaml`: generated OpenAPI 3.0 client contract.
- `pkg/client/generated/client_with_response.go`: generated 413 response decoding.
- `pkg/client/generated/responses.go`: generated 413 response type and field.
- `web/src/lib/api/generated/schema.d.ts`: generated browser API contract.

---

### Task 1: Bound Aggregate Values and Explicit Media

**Files:**
- Modify: `internal/store/organization_profile.go:147-425`
- Test: `internal/store/organization_profile_test.go:1-185`

**Interfaces:**
- Produces: `store.MaxOrganizationProfileValues = 200`
- Produces: `store.MaxOrganizationProfileMediaBytes int64 = 32 << 20`
- Produces: `store.ErrOrganizationProfileTooLarge`
- Produces: `preparedOrganizationProfile.explicitMediaBytes int64` for Task 2.

- [ ] **Step 1: Write failing store limit tests**

Add `strconv` to the test imports, then add these tests:

```go
func TestReplaceOrganizationProfileBoundsAggregateValues(t *testing.T) {
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(t, err)

	categories := make([]store.OrganizationCategoryInput, store.MaxOrganizationProfileValues)
	for i := range categories {
		categories[i] = store.OrganizationCategoryInput{
			Category: "category-" + strconv.Itoa(i),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}
	}
	input := store.OrganizationProfileInput{Categories: categories}
	first, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, input)
	require.NoError(t, err)
	require.Len(t, first.Categories, store.MaxOrganizationProfileValues)

	second, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, first.Organization.Revision, input)
	require.NoError(t, err)
	require.Len(t, second.Categories, store.MaxOrganizationProfileValues)
	for i := range first.Categories {
		assert.Equal(t, first.Categories[i].Envelope.ID, second.Categories[i].Envelope.ID)
	}

	oversized := append([]store.OrganizationCategoryInput(nil), categories...)
	oversized = append(oversized, store.OrganizationCategoryInput{
		Category: "one-too-many",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, second.Organization.Revision,
		store.OrganizationProfileInput{Categories: oversized})
	require.ErrorIs(t, err, store.ErrOrganizationProfileTooLarge)

	unchanged, err := st.GetOrganizationContext(ctx, organization.ID)
	require.NoError(t, err)
	assert.Equal(t, second.Organization.Revision, unchanged.Revision)
}

func TestReplaceOrganizationProfileBoundsExplicitMediaTotal(t *testing.T) {
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(t, err)

	shared := make([]byte, store.MaxPersonMediaBytes)
	media := make([]store.OrganizationMediaInput, 0, 5)
	for i := range 4 {
		ordinal := i
		media = append(media, store.OrganizationMediaInput{
			MediaKind: store.PersonMediaPhoto,
			Data:      shared,
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceUser, Ordinal: &ordinal,
			},
		})
	}
	media = append(media, store.OrganizationMediaInput{
		MediaKind: store.PersonMediaPhoto,
		Data:      []byte{1},
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceUser, Ordinal: new(4),
		},
	})

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision,
		store.OrganizationProfileInput{Media: media})
	require.ErrorIs(t, err, store.ErrOrganizationProfileTooLarge)

	unchanged, err := st.GetOrganizationContext(ctx, organization.ID)
	require.NoError(t, err)
	assert.Equal(t, organization.Revision, unchanged.Revision)
}
```

- [ ] **Step 2: Run the new tests and verify the red state**

Run:

```bash
go test -count=1 -tags "fts5 sqlite_vec" ./internal/store \
  -run 'TestReplaceOrganizationProfileBounds(AggregateValues|ExplicitMediaTotal)$'
```

Expected: compilation fails because `MaxOrganizationProfileValues` and
`ErrOrganizationProfileTooLarge` do not exist.

- [ ] **Step 3: Add the store constants, error, and one top-level validator**

Add the limits and error near `OrganizationProfileInput`:

```go
const (
	MaxOrganizationProfileValues           = 200
	MaxOrganizationProfileMediaBytes int64 = 32 << 20
)

var ErrOrganizationProfileTooLarge = errors.New(
	"organization profile exceeds the aggregate size limit")
```

Add the explicit-byte total to the prepared value:

```go
type preparedOrganizationProfile struct {
	input              OrganizationProfileInput
	explicitMediaBytes int64
	nameKeys           []string
	identifierKeys     []string
	addressKeys        []string
	contacts           []preparedOrganizationContact
	contactKeys        []string
	mediaKeys          []string
	categoryKeys       []string
}
```

Add this validator before `prepareOrganizationProfileContext`:

```go
func validateOrganizationProfileLimits(input OrganizationProfileInput) (int64, error) {
	valueCount := len(input.Names) + len(input.Identifiers) + len(input.Addresses) +
		len(input.ContactPoints) + len(input.Media) + len(input.Categories)
	if valueCount > MaxOrganizationProfileValues {
		return 0, fmt.Errorf(
			"%w: profile contains %d values; maximum is %d",
			ErrOrganizationProfileTooLarge, valueCount, MaxOrganizationProfileValues)
	}

	var explicitMediaBytes int64
	for i := range input.Media {
		size := int64(len(input.Media[i].Data))
		if size > MaxOrganizationProfileMediaBytes-explicitMediaBytes {
			return 0, fmt.Errorf(
				"%w: inline media exceeds %d bytes",
				ErrOrganizationProfileTooLarge, MaxOrganizationProfileMediaBytes)
		}
		explicitMediaBytes += size
	}
	return explicitMediaBytes, nil
}
```

Call it as the first operation in preparation, before allocating maps or
entering any collection loop:

```go
func (s *Store) prepareOrganizationProfileContext(
	ctx context.Context, input OrganizationProfileInput,
) (*preparedOrganizationProfile, error) {
	explicitMediaBytes, err := validateOrganizationProfileLimits(input)
	if err != nil {
		return nil, err
	}
	prepared := &preparedOrganizationProfile{
		input: input, explicitMediaBytes: explicitMediaBytes,
	}
	// Existing per-collection validation follows unchanged.
```

Keep the existing per-value `MaxPersonMediaBytes` check in the media loop.

- [ ] **Step 4: Run focused and package tests**

Run:

```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/store
go test -count=1 -tags "fts5 sqlite_vec" ./internal/store \
  -run 'TestReplaceOrganizationProfile(BoundsAggregateValues|BoundsExplicitMediaTotal|RoundTripsEveryCollectionAndKeepsStableRows)$'
go test -count=1 -tags "fts5 sqlite_vec" ./internal/store
```

Expected: PASS.

- [ ] **Step 5: Commit the store input bounds**

```bash
git add internal/store/organization_profile.go internal/store/organization_profile_test.go
git commit
```

Use subject: `fix(organizations): bound profile replacement inputs`.

---

### Task 2: Preflight and Cache Retained Media

**Files:**
- Modify: `internal/store/organization_profile.go:555-575,1092-1132`
- Test: `internal/store/organization_profile_test.go`

**Interfaces:**
- Consumes: `preparedOrganizationProfile.explicitMediaBytes` from Task 1.
- Consumes: `MaxOrganizationProfileMediaBytes` and `ErrOrganizationProfileTooLarge` from Task 1.
- Produces: retained-media resolution that performs all byte-budget checks before its first `SELECT data`.

- [ ] **Step 1: Write the failing retained-media expansion test**

Add this test:

```go
func TestReplaceOrganizationProfileBoundsRetainedMediaExpansion(t *testing.T) {
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(t, err)

	data := make([]byte, 256<<10)
	first, err := st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision,
		store.OrganizationProfileInput{Media: []store.OrganizationMediaInput{{
			MediaKind: store.PersonMediaLogo,
			Data:      data,
			Envelope:  store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		}}})
	require.NoError(t, err)
	require.Len(t, first.Media, 1)
	require.NotNil(t, first.Media[0].ContentHash)

	copyCount := int(store.MaxOrganizationProfileMediaBytes/int64(len(data))) + 1
	media := make([]store.OrganizationMediaInput, copyCount)
	for i := range media {
		ordinal := i
		media[i] = store.OrganizationMediaInput{
			MediaKind:   store.PersonMediaLogo,
			ContentHash: first.Media[0].ContentHash,
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceUser, Ordinal: &ordinal,
			},
		}
	}

	_, err = st.ReplaceOrganizationProfileContext(
		ctx, organization.ID, first.Organization.Revision,
		store.OrganizationProfileInput{Media: media})
	require.ErrorIs(t, err, store.ErrOrganizationProfileTooLarge)

	unchanged, err := st.GetOrganizationProfileContext(ctx, organization.ID, false)
	require.NoError(t, err)
	require.Len(t, unchanged.Media, 1)
	assert.Equal(t, first.Organization.Revision, unchanged.Organization.Revision)
	stored, _, err := st.ReadOrganizationMediaDataContext(
		ctx, organization.ID, unchanged.Media[0].Envelope.ID)
	require.NoError(t, err)
	assert.Equal(t, data, stored)
}
```

- [ ] **Step 2: Run the regression and verify it fails on expansion**

Run:

```bash
go test -count=1 -tags "fts5 sqlite_vec" ./internal/store \
  -run '^TestReplaceOrganizationProfileBoundsRetainedMediaExpansion$'
```

Expected: FAIL because the replacement succeeds instead of returning
`ErrOrganizationProfileTooLarge`.

- [ ] **Step 3: Preflight metadata and cache one blob per hash**

Pass the explicit byte total into retention resolution:

```go
if err := s.resolveOrganizationMediaRetentionTx(
	ctx, tx, current.Media, prepared.input.Media, prepared.explicitMediaBytes,
); err != nil {
	return err
}
```

Replace `resolveOrganizationMediaRetentionTx` with a two-phase implementation:

```go
func (s *Store) resolveOrganizationMediaRetentionTx(
	ctx context.Context, tx *loggedTx,
	currentMedia []OrganizationMedia, inputs []OrganizationMediaInput,
	explicitMediaBytes int64,
) error {
	type retentionSource struct {
		id       int64
		byteSize *int64
	}
	sources := make(map[string]retentionSource, len(currentMedia))
	for _, row := range currentMedia {
		if !row.HasData || row.ContentHash == nil {
			continue
		}
		if _, exists := sources[*row.ContentHash]; !exists {
			sources[*row.ContentHash] = retentionSource{
				id: row.Envelope.ID, byteSize: row.ByteSize,
			}
		}
	}

	retainedBytes := explicitMediaBytes
	for i := range inputs {
		input := &inputs[i]
		if len(input.Data) > 0 || input.ContentHash == nil {
			continue
		}
		source, exists := sources[*input.ContentHash]
		if !exists {
			return fmt.Errorf(
				"%w: media[%d].content_hash %q does not match an active media row; re-send data or drop content_hash",
				ErrOrganizationInvalid, i, *input.ContentHash)
		}
		if source.byteSize == nil || *source.byteSize <= 0 {
			return fmt.Errorf(
				"active organization media %d has invalid byte_size", source.id)
		}
		if *source.byteSize > MaxOrganizationProfileMediaBytes-retainedBytes {
			return fmt.Errorf(
				"%w: inline media exceeds %d bytes",
				ErrOrganizationProfileTooLarge, MaxOrganizationProfileMediaBytes)
		}
		retainedBytes += *source.byteSize
	}

	dataByHash := make(map[string][]byte, len(sources))
	for i := range inputs {
		input := &inputs[i]
		if len(input.Data) > 0 || input.ContentHash == nil {
			continue
		}
		contentHash := *input.ContentHash
		data, loaded := dataByHash[contentHash]
		if !loaded {
			source := sources[contentHash]
			if err := tx.QueryRowContext(ctx,
				`SELECT data FROM organization_media WHERE id = ?`, source.id,
			).Scan(&data); err != nil {
				return fmt.Errorf("load retained media %d content: %w", source.id, err)
			}
			if int64(len(data)) != *source.byteSize {
				return fmt.Errorf(
					"active organization media %d byte_size does not match stored data",
					source.id)
			}
			dataByHash[contentHash] = data
		}
		input.Data = data
	}
	return nil
}
```

This deliberately counts a shared hash once per desired row but keeps one
loaded byte slice per distinct hash.

- [ ] **Step 4: Run retained-media and existing round-trip tests**

Run:

```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/store ./internal/api
go test -count=1 -tags "fts5 sqlite_vec" ./internal/store \
  -run 'TestReplaceOrganizationProfile(BoundsRetainedMediaExpansion|RoundTripsEveryCollectionAndKeepsStableRows)$'
go test -count=1 -tags "fts5 sqlite_vec" ./internal/api \
  -run 'TestOrganizationHTTPProfilePut(RetainsInlineMediaViaContentHash|EditsInlineMediaMetadataViaContentHash|RetainsInlineOnlyMediaWithoutURI)$'
```

Expected: PASS.

- [ ] **Step 5: Commit retained-media preflight and caching**

```bash
git add internal/store/organization_profile.go internal/store/organization_profile_test.go
git commit
```

Use subject: `fix(organizations): cap retained profile media`.

---

### Task 3: Publish the HTTP 413 Contract

**Files:**
- Modify: `internal/api/organizations.go:257-265,800-850`
- Test: `internal/api/organizations_test.go`
- Test: `internal/api/openapi_test.go`
- Regenerate: `api/openapi.yaml`
- Regenerate: `pkg/client/openapi.yaml`
- Regenerate: `pkg/client/generated/client_with_response.go`
- Regenerate: `pkg/client/generated/responses.go`
- Regenerate: `web/src/lib/api/generated/schema.d.ts`

**Interfaces:**
- Consumes: `store.ErrOrganizationProfileTooLarge` and `store.MaxOrganizationProfileValues`.
- Produces: HTTP 413 with error code `organization_profile_too_large`.
- Produces: a PUT operation that documents status 413 and the 200-value aggregate limit.

- [ ] **Step 1: Write failing HTTP and OpenAPI tests**

Add this API test to `internal/api/organizations_test.go`:

```go
func TestOrganizationHTTPProfileRejectsTooManyValues(t *testing.T) {
	srv := newOrganizationTestServer(t)
	createdResponse := organizationRequest(t, srv, http.MethodPost, organizationsPath,
		[]byte(`{"name":"Example Org","kind":"company"}`), "")
	require.Equal(t, http.StatusCreated, createdResponse.Code)
	var created store.Organization
	require.NoError(t, json.Unmarshal(createdResponse.Body.Bytes(), &created))

	categories := make([]OrganizationCategoryBody, store.MaxOrganizationProfileValues+1)
	for i := range categories {
		categories[i] = OrganizationCategoryBody{
			OrganizationEnvelopeBody: OrganizationEnvelopeBody{
				Source: string(store.ProvenanceUser),
			},
			Category: fmt.Sprintf("category-%d", i),
		}
	}
	body, err := json.Marshal(OrganizationProfileBody{Categories: categories})
	require.NoError(t, err)
	response := organizationRequest(t, srv, http.MethodPut,
		fmt.Sprintf("%s/%d/profile", organizationsPath, created.ID), body,
		createdResponse.Header().Get("ETag"))
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())

	apiError := decodeErrorEnvelope(t, response)
	assert.Equal(t, "organization_profile_too_large", apiError.Error)
}
```

Add this contract test to `internal/api/openapi_test.go`:

```go
func TestOpenAPIOrganizationProfilePutDocumentsLimits(t *testing.T) {
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/organizations/{id}/profile"]
	require.NotNil(t, path)
	require.NotNil(t, path.Put)
	assert.Contains(t, path.Put.Description, "200")

	response := path.Put.Responses[httpStatusKey(http.StatusRequestEntityTooLarge)]
	require.NotNil(t, response)
	media := response.Content["application/json"]
	require.NotNil(t, media)
	require.NotNil(t, media.Schema)
	assert.Equal(t, "#/components/schemas/ErrorResponse", media.Schema.Ref)
}
```

- [ ] **Step 2: Run the API tests and verify the red state**

Run:

```bash
go test -count=1 -tags "fts5 sqlite_vec" ./internal/api \
  -run 'Test(OrganizationHTTPProfileRejectsTooManyValues|OpenAPIOrganizationProfilePutDocumentsLimits)$'
```

Expected: the HTTP test receives 500 and the OpenAPI test finds no 413
response.

- [ ] **Step 3: Map the typed error and document the operation**

Set the operation description and add 413 to its generated error responses:

```go
profile := rawAPIV1Operation(
	"putOrganizationProfile", http.MethodPut, "/organizations/{id}/profile",
	"Replace organization profile collections",
)
profile.Description = fmt.Sprintf(
	"Replaces all structured organization profile collections with at most %d total values.",
	store.MaxOrganizationProfileValues,
)
// Existing parameters, body, and success response remain unchanged.
addErrorResponses(api, profile.Responses,
	http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
	http.StatusPreconditionRequired, http.StatusRequestEntityTooLarge,
	http.StatusServiceUnavailable,
)
```

Add the typed mapping before the generic organization validation cases:

```go
case errors.Is(err, store.ErrOrganizationProfileTooLarge):
	writeError(w, http.StatusRequestEntityTooLarge,
		"organization_profile_too_large", err.Error())
```

Do not call the limit validator from `organizationProfileInput`; the store is
the single enforcement point.

- [ ] **Step 4: Run focused tests and regenerate API clients**

Run:

```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./internal/api
go test -count=1 -tags "fts5 sqlite_vec" ./internal/api \
  -run 'Test(OrganizationHTTPProfileRejectsTooManyValues|OpenAPIOrganizationProfilePutDocumentsLimits)$'
make api-generate
make web-generate
make openapi-check
```

Expected: PASS, and generated changes are limited to the declared OpenAPI and
client artifacts.

- [ ] **Step 5: Commit the HTTP contract and generated artifacts**

```bash
git add internal/api/organizations.go internal/api/organizations_test.go \
  internal/api/openapi_test.go api/openapi.yaml pkg/client/openapi.yaml \
  pkg/client/generated web/src/lib/api/generated/schema.d.ts
git commit
```

Use subject: `fix(api): report oversized organization profiles`.

---

### Task 4: Verify and Publish the Branch

**Files:**
- Review: all files changed from `main...HEAD`.

**Interfaces:**
- Consumes: the complete implementation from Tasks 1-3.
- Produces: a clean, verified branch and a generic pull request.

- [ ] **Step 1: Format and vet the Go tree**

Run:

```bash
go fmt ./...
go vet -tags "fts5 sqlite_vec" ./...
```

Expected: no formatting diff outside the intended files and no vet failures.

- [ ] **Step 2: Run lint and generated-contract checks**

Run:

```bash
make lint-ci
make openapi-check
make web-check
```

Expected: PASS.

- [ ] **Step 3: Run the complete tagged Go suite in isolated scratch state**

Run:

```bash
umask 077
scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/msgvault-profile-test.XXXXXX")
scratch_name=$(basename "$scratch_dir")
trap 'mv -- "$scratch_dir" "${HOME}/.Trash/$scratch_name"' EXIT
mkdir -p "$scratch_dir/msgvault" "$scratch_dir/config" \
  "$scratch_dir/cache" "$scratch_dir/data"
env -u MSGVAULT_EMBED_API_KEY -u MSGVAULT_TEST_DB \
  -u GIT_CONFIG -u GIT_CONFIG_GLOBAL -u GIT_CONFIG_SYSTEM \
  MSGVAULT_HOME="$scratch_dir/msgvault" \
  XDG_CONFIG_HOME="$scratch_dir/config" \
  XDG_CACHE_HOME="$scratch_dir/cache" \
  XDG_DATA_HOME="$scratch_dir/data" \
  make test
```

Expected: all packages PASS with `-tags "fts5 sqlite_vec"` supplied by the
Makefile.

- [ ] **Step 4: Review scope and public text**

Run:

```bash
git status --short
git diff --check main...HEAD
git diff --stat main...HEAD
git log --oneline main..HEAD
```

Expected: only the design, plan, implementation, tests, and generated API
artifacts are present. Run the private-data scrub workflow before any push.

- [ ] **Step 5: Push and open the pull request**

Use title:

```text
fix(organizations): bound profile replacement work
```

Use a concise body that states only that organization profile writes now have
aggregate value and retained-media limits, return HTTP 413 when exceeded, and
reuse each retained blob load. Do not include a validation/test-plan section.

```bash
git push --set-upstream origin fix/organization-profile-limits
gh pr create --base main \
  --title 'fix(organizations): bound profile replacement work' \
  --body 'Organization profile replacements now reject more than 200 values or 32 MiB of logical inline media with HTTP 413. Retained content hashes are budgeted from stored metadata, and each distinct blob is loaded once per request.'
```
