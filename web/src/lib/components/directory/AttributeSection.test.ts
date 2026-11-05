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
  render(AttributeSection, { controller });
  return controller;
}

describe('AttributeSection', () => {
  it('keeps current imported provenance reachable while concealing sensitive values until reveal', async () => {
    const sensitive = definition();
    const current = personValue(sensitive, 19, { type: 'text', text: 'synthetic current secret' }, 0, { source: 'carddav_import', source_ref: 'source-card-7', actor: 'synthetic-importer', confidence: 0.8 });
    const historical = personValue(sensitive, 17, { type: 'text', text: 'synthetic historical secret' }, 0, {
      active_until: '2026-07-01T00:00:00Z',
      superseded_at: '2026-07-01T00:00:00Z',
    });
    renderSection(vi.fn(), [{ definition: sensitive, current: [current], history: [historical] }]);

    expect(screen.getByRole('heading', { name: 'Private note' })).toBeDefined();
    expect(screen.getByText('1 earlier value')).toBeDefined();
    expect(screen.getAllByText('Hidden')).toHaveLength(2);
    const currentRow = screen.getByText(/Reference: source-card-7/).closest('li')!;
    expect(currentRow.textContent).toContain('Actor: synthetic-importer');
    expect(currentRow.textContent).toContain('Confidence: 0.8');
    expect(currentRow.textContent).toContain('Valid from:');
    expect(screen.getByText('1 earlier value').closest('details')?.textContent).toContain('Valid until:');
    expect(document.body.innerHTML).not.toContain('synthetic current secret');
    expect(screen.queryByText('synthetic historical secret')).toBeNull();
    expect(document.body.innerHTML).not.toContain('synthetic historical secret');

    const reveal = screen.getByRole('button', { name: 'Reveal Private note values' });
    reveal.focus();
    await fireEvent.click(reveal);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Hide Private note values' }));
    expect(screen.getByText('synthetic historical secret')).toBeDefined();
    expect(screen.getByText('synthetic current secret')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Hide Private note values' }));
    expect(document.body.innerHTML).not.toContain('synthetic historical secret');
    expect(screen.getAllByText('Hidden')).toHaveLength(2);
    expect(document.body.innerHTML).not.toContain('synthetic current secret');
  });

  it('renders collapsed attribute history with choice labels and removed rows', () => {
    const field = definition({
      label: 'Portable field', is_sensitive: false, cardinality: 'multi',
      options: { choices: [{ value: 'one', label: 'One' }, { value: 'two', label: 'Two' }] },
    });
    const current = personValue(field, 31, { type: 'text', text: 'one' }, 2);
    const historical = personValue(field, 29, { type: 'text', text: 'two' }, 2, {
      source: 'vcard_import',
      source_ref: 'synthetic-source',
      active_until: when,
      superseded_at: when,
    });

    renderSection(vi.fn(), [{ definition: field, current: [current], history: [current, historical] }]);

    const history = screen.getByText('1 earlier value').closest('details');
    expect(history).not.toBeNull();
    expect(history?.hasAttribute('open')).toBe(false);
    const row = screen.getByText('Two').closest('li')!;
    expect(row.textContent).toMatch(/Valid until: (Jul|Aug) \d{1,2}, 2026/);
  });

  it.each([
    { active_until: '2026-08-02T00:00:00Z' },
    { superseded_at: '2026-08-03T00:00:00Z' },
    { active_until: '2026-08-02T00:00:00Z', superseded_at: '2026-08-03T00:00:00Z' },
  ])('shows each closed attribute interval once in provenance: %j', interval => {
    const field = definition({ is_sensitive: false });
    const historical = personValue(field, 29, { type: 'text', text: 'Earlier note' }, 0, interval);
    renderSection(vi.fn(), [{ definition: field, current: [], history: [historical] }]);

    const row = screen.getByText('Earlier note').closest('li')!;
    expect(row.closest('details')).not.toBeNull();
    const metadata = row.querySelector('.meta')!.textContent!.trim();
    expect(metadata.split(' · ').map(part => part.split(': ')[0])).toEqual([
      'You', 'Actor', 'Valid from', ...(interval.active_until ? ['Valid until'] : []), ...(interval.superseded_at ? ['Superseded'] : []), 'Created',
    ]);
    if (interval.active_until) expect(metadata).toMatch(/Valid until: (Aug 1|Aug 2), 2026/);
    if (interval.superseded_at) expect(metadata).toMatch(/Superseded: (Aug 2|Aug 3), 2026/);
  });

  it('filters fields by universal ID', async () => {
    const privateField = definition();
    const other = definition({ id: 9, universal_id: '00000000-0000-4000-8000-000000000009', label: 'Other note', slug: 'other', display_order: 20 });
    const groups = [
      { definition: privateField, current: [personValue(privateField, 51, { type: 'text', text: 'First secret' })], history: [] },
      { definition: other, current: [personValue(other, 52, { type: 'text', text: 'Other secret' })], history: [] },
    ];
    const controller = new DirectoryProfileController(createAPIClient(vi.fn()), 7, {
      attributes: { person_id: 7, attributes: groups }, definitions: { definitions: [privateField, other] }, etags: {}, errors: {},
    });
    render(AttributeSection, { controller, universalIDs: [privateField.universal_id] });
    expect(screen.queryByRole('heading', { name: 'Other note' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Private note values' }));
    expect(screen.getByText('First secret')).toBeDefined();
    expect(screen.queryByText('Other secret')).toBeNull();
    controller.destroy();
  });

  it('gives History distinct headings and separate reveals', async () => {
    const privateField = definition();
    const controller = new DirectoryProfileController(createAPIClient(vi.fn()), 7, {
      attributes: { person_id: 7, attributes: [{ definition: privateField, current: [personValue(privateField, 51, { type: 'text', text: 'First secret' })], history: [] }] },
      definitions: { definitions: [privateField] }, etags: {}, errors: {},
    });
    const facts = render(AttributeSection, { controller });
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Private note values' }));
    expect(facts.container.textContent).toContain('First secret');
    const history = render(AttributeSection, { controller });
    const headings = document.querySelectorAll('h3');
    expect(new Set(Array.from(headings, heading => heading.id)).size).toBe(headings.length);
    expect(history.container.textContent).not.toContain('First secret');
    controller.destroy();
  });
});
