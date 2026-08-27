package store

import (
	"context"

	"go.kenn.io/msgvault/internal/personfacts"
)

// BuildPersonFactCatalogContext maps person attribute definitions into the
// provider-neutral fact catalog.
func (s *Store) BuildPersonFactCatalogContext(
	ctx context.Context, includeSensitive bool,
) (personfacts.Catalog, error) {
	return s.buildPersonFactCatalogContext(ctx, s.db, includeSensitive)
}

func (s *Store) buildPersonFactCatalogContext(
	ctx context.Context, queryer contextRowsQuerier, includeSensitive bool,
) (personfacts.Catalog, error) {
	definitions, err := s.listAttributeDefinitionsContext(ctx, queryer, AttributeDefinitionFilter{
		ObjectType:    AttributeObjectPerson,
		IncludeHidden: true,
	})
	if err != nil {
		return personfacts.Catalog{}, err
	}
	mapped := make([]personfacts.Definition, 0, len(definitions))
	for _, definition := range definitions {
		mapped = append(mapped, personfacts.Definition{
			UniversalID:  definition.UniversalID,
			Slug:         definition.Slug,
			Description:  personFactStringValue(definition.Description),
			ValueType:    personfacts.ValueType(definition.ValueType),
			Cardinality:  personfacts.Cardinality(definition.Cardinality),
			RecordTarget: personFactStringValue(definition.RecordTarget),
			MaxLength:    personFactMaxLength(definition.Options),
			Choices:      personFactChoices(definition.Options),
			Sensitive:    definition.IsSensitive,
			Active:       definition.IsActive,
			APIMutable:   definition.APIMutable,
			Derived:      definition.DerivedSource != nil,
		})
	}
	return personfacts.BuildCatalog(mapped, personfacts.CatalogOptions{
		IncludeSensitive: includeSensitive,
	})
}

func personFactMaxLength(options *AttributeOptions) int {
	if options == nil {
		return 0
	}
	return options.MaxLength
}

func personFactChoices(options *AttributeOptions) []personfacts.ChoiceDescriptor {
	if options == nil {
		return nil
	}
	choices := make([]personfacts.ChoiceDescriptor, len(options.Choices))
	for i, choice := range options.Choices {
		choices[i] = personfacts.ChoiceDescriptor{Value: choice.Value, Label: choice.Label}
	}
	return choices
}

func personFactStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
