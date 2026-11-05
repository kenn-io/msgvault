import type { AttributeDefinition } from '../api/generated/models';

const when = '2026-08-01T00:00:00Z';

export function definition(overrides: Partial<AttributeDefinition> = {}): AttributeDefinition {
  return {
    id: 8,
    universal_id: '00000000-0000-4000-8000-000000000008',
    object_type: 'person',
    slug: 'private_note',
    label: 'Private note',
    description: 'A deliberately sensitive note',
    value_type: 'text',
    field_type: 'textarea',
    cardinality: 'single',
    display_order: 10,
    is_required: false,
    ownership: 'user',
    ui_creatable: true,
    ui_editable: true,
    api_mutable: true,
    is_searchable: false,
    is_sensitive: true,
    is_audited: true,
    is_deletable: true,
    history_exempt: false,
    is_active: true,
    revision: 1,
    created_at: when,
    updated_at: when,
    ...overrides,
  };
}
