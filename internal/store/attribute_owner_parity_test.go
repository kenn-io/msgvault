package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// attributeOwnerCase drives person and organization values through one set of
// assertions, so a history or supersede rule cannot hold for only one owner.
type attributeOwnerCase struct {
	name      string
	setup     func(t *testing.T, st *store.Store) (ownerID int64)
	define    func(slug string) store.AttributeDefinitionInput
	set       func(ctx context.Context, st *store.Store, in attributeParityWrite) (*attributeParityResult, error)
	supersede func(ctx context.Context, st *store.Store, in attributeParityWrite) (*attributeParityResult, error)
	list      func(ctx context.Context, st *store.Store, ownerID int64, slug string, history bool) ([]attributeParityValue, error)
	// typedConflict reports whether a lost compare-and-swap carries the current value.
	typedConflict bool
}

type attributeParityWrite struct {
	ownerID         int64
	slug            string
	text            string
	ordinal         *int64
	activeFrom      *time.Time
	expectedValueID *int64
	dryRun          bool
}

type attributeParityValue struct {
	id, ownerID, ordinal int64
	text                 string
	current              bool
	createdAt            time.Time
	supersededAt         *time.Time
}

type attributeParityResult struct {
	value, superseded *attributeParityValue
	dryRun            bool
}

func personParityValue(v *store.PersonAttributeValue) *attributeParityValue {
	if v == nil {
		return nil
	}
	return &attributeParityValue{
		id: v.ID, ownerID: v.PersonID, ordinal: v.Ordinal, text: *v.Value.Text,
		current:   v.ActiveUntil == nil && v.SupersededAt == nil,
		createdAt: v.CreatedAt, supersededAt: v.SupersededAt,
	}
}

func organizationParityValue(v *store.OrganizationAttributeValue) *attributeParityValue {
	if v == nil {
		return nil
	}
	return &attributeParityValue{
		id: v.ID, ownerID: v.OrganizationID, ordinal: v.Ordinal, text: *v.Value.Text,
		current:   v.ActiveUntil == nil && v.SupersededAt == nil,
		createdAt: v.CreatedAt, supersededAt: v.SupersededAt,
	}
}

func attributeOwnerCases() []attributeOwnerCase {
	return []attributeOwnerCase{
		{
			name:  "person",
			setup: mustAttributePerson,
			define: func(slug string) store.AttributeDefinitionInput {
				input := personTextDefinition(slug)
				input.Cardinality = store.AttributeCardinalityMulti
				return input
			},
			set: func(ctx context.Context, st *store.Store, in attributeParityWrite) (*attributeParityResult, error) {
				w, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
					PersonID: in.ownerID, DefinitionSlug: in.slug, Ordinal: in.ordinal,
					Value: textAttributeValue(in.text), ActiveFrom: in.activeFrom,
					Source: store.ProvenanceUser, ExpectedValueID: in.expectedValueID, DryRun: in.dryRun,
				})
				if err != nil {
					return nil, err
				}
				return &attributeParityResult{personParityValue(w.Value), personParityValue(w.Superseded), w.DryRun}, nil
			},
			supersede: func(ctx context.Context, st *store.Store, in attributeParityWrite) (*attributeParityResult, error) {
				w, err := st.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{
					PersonID: in.ownerID, DefinitionSlug: in.slug, Ordinal: in.ordinal,
					ExpectedValueID: in.expectedValueID, DryRun: in.dryRun,
				})
				if err != nil {
					return nil, err
				}
				return &attributeParityResult{personParityValue(w.Value), personParityValue(w.Superseded), w.DryRun}, nil
			},
			list: func(ctx context.Context, st *store.Store, ownerID int64, slug string, history bool) ([]attributeParityValue, error) {
				values, err := st.ListPersonAttributeValuesContext(ctx, ownerID,
					store.PersonAttributeQuery{DefinitionSlug: slug, IncludeHistory: history})
				out := make([]attributeParityValue, 0, len(values))
				for i := range values {
					out = append(out, *personParityValue(&values[i]))
				}
				return out, err
			},
			typedConflict: true,
		},
		{
			name: "organization",
			setup: func(t *testing.T, st *store.Store) int64 {
				t.Helper()
				return mustAttributeOrganization(t, st).ID
			},
			define: func(slug string) store.AttributeDefinitionInput {
				input := organizationTextDefinition(slug)
				input.Cardinality = store.AttributeCardinalityMulti
				return input
			},
			set: func(ctx context.Context, st *store.Store, in attributeParityWrite) (*attributeParityResult, error) {
				w, err := st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
					OrganizationID: in.ownerID, DefinitionSlug: in.slug, Ordinal: in.ordinal,
					Value: textAttributeValue(in.text), ActiveFrom: in.activeFrom,
					Source: store.ProvenanceUser, ExpectedValueID: in.expectedValueID, DryRun: in.dryRun,
				})
				if err != nil {
					return nil, err
				}
				return &attributeParityResult{organizationParityValue(w.Value), organizationParityValue(w.Superseded), w.DryRun}, nil
			},
			supersede: func(ctx context.Context, st *store.Store, in attributeParityWrite) (*attributeParityResult, error) {
				w, err := st.SupersedeOrganizationAttributeValueContext(ctx, store.OrganizationAttributeSupersedeInput{
					OrganizationID: in.ownerID, DefinitionSlug: in.slug, Ordinal: in.ordinal,
					ExpectedValueID: in.expectedValueID, DryRun: in.dryRun,
				})
				if err != nil {
					return nil, err
				}
				return &attributeParityResult{organizationParityValue(w.Value), organizationParityValue(w.Superseded), w.DryRun}, nil
			},
			list: func(ctx context.Context, st *store.Store, ownerID int64, slug string, history bool) ([]attributeParityValue, error) {
				values, err := st.ListOrganizationAttributeValuesContext(ctx, ownerID,
					store.OrganizationAttributeQuery{DefinitionSlug: slug, IncludeHistory: history})
				out := make([]attributeParityValue, 0, len(values))
				for i := range values {
					out = append(out, *organizationParityValue(&values[i]))
				}
				return out, err
			},
		},
	}
}

func TestAttributeOwnersShareHistoryAndSupersede(t *testing.T) {
	for _, owner := range attributeOwnerCases() {
		t.Run(owner.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			ctx := context.Background()
			st := testutil.NewTestStore(t)
			ownerID := owner.setup(t, st)
			_, err := st.CreateAttributeDefinitionContext(ctx, owner.define("aliases"))
			require.NoError(err)
			write := func(in attributeParityWrite) attributeParityWrite {
				in.ownerID, in.slug = ownerID, "aliases"
				return in
			}
			firstAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			secondAt := firstAt.Add(time.Hour)
			zero := int64(0)

			first, err := owner.set(ctx, st, write(attributeParityWrite{text: "first", activeFrom: &firstAt}))
			require.NoError(err)
			assert.Equal(int64(0), first.value.ordinal)
			assert.Equal(ownerID, first.value.ownerID)
			assert.Nil(first.superseded)
			appended, err := owner.set(ctx, st, write(attributeParityWrite{text: "appended", activeFrom: &firstAt}))
			require.NoError(err)
			assert.Equal(int64(1), appended.value.ordinal, "an unset ordinal appends a new slot")

			_, err = owner.set(ctx, st, write(attributeParityWrite{
				text: "stale", ordinal: &zero, expectedValueID: &appended.value.id,
			}))
			require.ErrorIs(err, store.ErrAttributeValueConflict)
			var typed *store.AttributeValueConflictError
			if owner.typedConflict {
				require.ErrorAs(err, &typed)
				assert.Equal(first.value.id, typed.CurrentValue.ID)
			} else {
				assert.NotErrorAs(err, &typed)
			}

			preview, err := owner.set(ctx, st, write(attributeParityWrite{
				text: "preview", ordinal: &zero, activeFrom: &secondAt, dryRun: true,
			}))
			require.NoError(err)
			assert.True(preview.dryRun)
			assert.Equal(int64(0), preview.value.id)
			assert.Equal(first.value.id, preview.superseded.id)

			second, err := owner.set(ctx, st, write(attributeParityWrite{
				text: "second", ordinal: &zero, activeFrom: &secondAt, expectedValueID: &first.value.id,
			}))
			require.NoError(err)
			assert.False(second.dryRun)
			require.NotNil(second.superseded)
			assert.Equal(first.value.id, second.superseded.id)
			if owner.typedConflict {
				assert.Equal(second.value.createdAt, *second.superseded.supersededAt,
					"a person replacement's audit time equals the closed row's supersede time")
			}

			cleared, err := owner.supersede(ctx, st, write(attributeParityWrite{ordinal: new(int64(1))}))
			require.NoError(err)
			assert.Nil(cleared.value)
			assert.Equal(appended.value.id, cleared.superseded.id)
			_, err = owner.supersede(ctx, st, write(attributeParityWrite{ordinal: new(int64(1))}))
			require.ErrorIs(err, store.ErrAttributeValueNotFound)
			reused, err := owner.set(ctx, st, write(attributeParityWrite{text: "after clear"}))
			require.NoError(err)
			assert.Equal(int64(2), reused.value.ordinal, "history keeps a cleared slot's ordinal reserved")

			current, err := owner.list(ctx, st, ownerID, "aliases", false)
			require.NoError(err)
			assert.Equal([]string{"second", "after clear"}, parityTexts(current))
			history, err := owner.list(ctx, st, ownerID, "", true)
			require.NoError(err)
			assert.Equal([]string{"second", "first", "appended", "after clear"}, parityTexts(history))
			assert.Equal([]bool{true, false, false, true}, parityCurrent(history))
			empty, err := owner.list(ctx, st, ownerID, "missing", true)
			require.NoError(err)
			assert.NotNil(empty)
			assert.Empty(empty)
		})
	}
}

func parityTexts(values []attributeParityValue) []string {
	texts := make([]string, 0, len(values))
	for _, value := range values {
		texts = append(texts, value.text)
	}
	return texts
}

func parityCurrent(values []attributeParityValue) []bool {
	current := make([]bool, 0, len(values))
	for _, value := range values {
		current = append(current, value.current)
	}
	return current
}

func TestOrganizationAttributeWritesRejectMergedOrganization(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	survivor := mustAttributeOrganization(t, st)
	losing, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{Name: "Other Org"})
	require.NoError(err)
	mustOrganizationAttributeDefinition(t, st, "industry_focus")
	_, err = st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
		OrganizationID: losing.ID, DefinitionSlug: "industry_focus",
		Value: textAttributeValue("before merge"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.MergeOrganizationsContext(ctx, survivor.ID, survivor.Revision, losing.ID, losing.Revision)
	require.NoError(err)

	_, err = st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
		OrganizationID: losing.ID, DefinitionSlug: "industry_focus",
		Value: textAttributeValue("after merge"), Source: store.ProvenanceUser,
	})
	require.ErrorIs(err, store.ErrOrganizationInvalid)
	_, err = st.SupersedeOrganizationAttributeValueContext(ctx, store.OrganizationAttributeSupersedeInput{
		OrganizationID: losing.ID, DefinitionSlug: "industry_focus",
	})
	require.ErrorIs(err, store.ErrOrganizationInvalid, "the row check runs before the current-value lookup")
}

func TestAttributeWriteErrorPrecedence(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	inactive := false
	for _, input := range []store.AttributeDefinitionInput{
		personTextDefinition("retired_person_field"), organizationTextDefinition("retired_org_field"),
	} {
		created, err := st.CreateAttributeDefinitionContext(ctx, input)
		require.NoError(err)
		_, err = st.UpdateAttributeDefinitionContext(ctx, created.ID, created.Revision,
			store.AttributeDefinitionUpdate{IsActive: &inactive})
		require.NoError(err)
	}
	mustOrganizationAttributeDefinition(t, st, "industry_focus")

	_, err := st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
		OrganizationID: 999999, DefinitionSlug: "retired_org_field",
		Value: textAttributeValue("x"), Source: store.ProvenanceUser,
	})
	require.ErrorIs(err, store.ErrAttributeDefinitionInactive,
		"organization definition checks run before the organization row lock")
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: 999999, DefinitionSlug: "retired_person_field",
		Value: textAttributeValue("x"), Source: store.ProvenanceUser,
	})
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{
		PersonID: 999999, DefinitionSlug: "retired_person_field",
	})
	require.ErrorIs(err, store.ErrAttributeValueNotFound, "only set checks that the person exists")
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: mustAttributePerson(t, st), DefinitionSlug: "industry_focus",
		Value: textAttributeValue("x"), Source: store.ProvenanceUser,
	})
	require.ErrorIs(err, store.ErrAttributeDefinitionNotFound)
}

func TestOrganizationAttributeCreatedAtUsesColumnDefault(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("PostgreSQL CURRENT_TIMESTAMP keeps sub-second precision")
	}
	organization := mustAttributeOrganization(t, st)
	mustOrganizationAttributeDefinition(t, st, "industry_focus")
	write, err := st.SetOrganizationAttributeValueContext(ctx, store.OrganizationAttributeValueInput{
		OrganizationID: organization.ID, DefinitionSlug: "industry_focus",
		Value: textAttributeValue("x"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.Zero(write.Value.CreatedAt.Nanosecond())
}
