import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type {
  AttributeDefinition as GeneratedAttributeDefinition,
  AttributeValue as GeneratedAttributeValue,
  PersonAttributeGroup as GeneratedPersonAttributeGroup,
  PersonAttributeValue as GeneratedPersonAttributeValue,
} from '../../api/generated/models';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import AttributeSection from './AttributeSection.svelte';

type AttributeDefinition = GeneratedAttributeDefinition;
type AttributeValue = GeneratedAttributeValue;
type PersonAttributeValue = GeneratedPersonAttributeValue;

const when = '2026-08-01T00:00:00Z';

afterEach(() => cleanup());

function definition(overrides: Partial<AttributeDefinition> = {}): AttributeDefinition {
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

function personValue(
  definitionValue: AttributeDefinition,
  id: number,
  value: AttributeValue,
  ordinal = 0,
  overrides: Partial<PersonAttributeValue> = {},
): PersonAttributeValue {
  return {
    id,
    person_id: 7,
    definition_id: definitionValue.id,
    definition_slug: definitionValue.slug,
    ordinal,
    value,
    active_from: when,
    created_at: when,
    source: 'user',
    actor: 'synthetic-user',
    ...overrides,
  };
}

function renderSection(
  fetchFn: typeof fetch,
  groups: GeneratedPersonAttributeGroup[],
  definitions: AttributeDefinition[] = groups.map((group) => group.definition),
) {
  const bundle = {
    attributes: { person_id: 7, attributes: groups },
    definitions: { definitions },
    etags: {},
    errors: {},
  } satisfies DirectoryReadBundle;
  const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle);
  render(AttributeSection, { controller, historyOnly: true });
  return controller;
}

describe('AttributeSection', () => {
  it('keeps sensitive history out of display and DOM until reveal', async () => {
    const sensitive = definition();
    const current = personValue(sensitive, 19, { type: 'text', text: 'synthetic current secret' });
    const historical = personValue(sensitive, 17, { type: 'text', text: 'synthetic historical secret' }, 0, {
      active_until: '2026-07-01T00:00:00Z',
      superseded_at: '2026-07-01T00:00:00Z',
    });
    renderSection(vi.fn(), [{ definition: sensitive, current: [current], history: [current, historical] }]);

    expect(screen.getByRole('heading', { name: 'Private note' })).toBeDefined();
    expect(screen.getByText('1 earlier value')).toBeDefined();
    expect(screen.getByText('Hidden')).toBeDefined();
    expect(screen.queryByText('synthetic historical secret')).toBeNull();
    expect(document.body.innerHTML).not.toContain('synthetic historical secret');

    const reveal = screen.getByRole('button', { name: 'Reveal Private note values' });
    reveal.focus();
    await fireEvent.click(reveal);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Hide Private note values' }));
    expect(screen.getByText('synthetic historical secret')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Hide Private note values' }));
    expect(document.body.innerHTML).not.toContain('synthetic historical secret');
    expect(screen.getByText('Hidden')).toBeDefined();
  });

  it('joins registry definitions to grouped values by universal ID and renders collapsed history', () => {
    const stale = definition({
      universal_id: 'portable-field-id',
      slug: 'portable_field',
      label: 'Stale label',
      is_sensitive: false,
    });
    const registry = definition({
      universal_id: 'portable-field-id',
      slug: 'portable_field',
      label: 'Portable field',
      description: 'Registry description',
      is_sensitive: false,
      cardinality: 'multi',
      options: {
        choices: [
          { value: 'one', label: 'One' },
          { value: 'two', label: 'Two' },
        ],
      },
    });
    const current = personValue(stale, 31, { type: 'text', text: 'one' }, 2);
    const historical = personValue(stale, 29, { type: 'text', text: 'two' }, 2, {
      source: 'vcard_import',
      source_ref: 'synthetic-source',
      active_until: when,
      superseded_at: when,
    });

    renderSection(vi.fn(), [{ definition: stale, current: [current], history: [current, historical] }], [registry]);

    expect(screen.getByRole('heading', { name: 'Portable field' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Stale label' })).toBeNull();
    const history = screen.getByText('1 earlier value').closest('details');
    expect(history).not.toBeNull();
    expect(history?.hasAttribute('open')).toBe(false);
    const row = screen.getByText('Two').closest('li')!;
    expect(row.textContent).toMatch(/Removed (Jul|Aug) \d{1,2}, 2026/);
    expect(row.textContent).not.toMatch(/Source:|Reference:|vcard_import|synthetic-source/);
  });
});
