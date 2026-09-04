import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import AttributeSection from './AttributeSection.svelte';

type AttributeDefinition = components['schemas']['AttributeDefinition'];
type AttributeValue = components['schemas']['AttributeValue'];
type PersonAttributeValue = components['schemas']['PersonAttributeValue'];

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
    ...overrides
  };
}

function personValue(
  definitionValue: AttributeDefinition,
  id: number,
  value: AttributeValue,
  ordinal = 0,
  overrides: Partial<PersonAttributeValue> = {}
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
    ...overrides
  };
}

function renderSection(
  fetchFn: typeof fetch,
  groups: components['schemas']['PersonAttributeGroup'][],
  definitions: AttributeDefinition[] = groups.map((group) => group.definition)
) {
  const bundle = {
    attributes: { person_id: 7, attributes: groups },
    definitions: { definitions },
    etags: {},
    errors: {}
  } satisfies DirectoryReadBundle;
  const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle);
  render(AttributeSection, { controller });
  return controller;
}

describe('AttributeSection', () => {
  it('keeps current and historical sensitive values out of display, editor, DOM, and accessible names until reveal', async () => {
    const sensitive = definition();
    const current = personValue(sensitive, 19, { type: 'text', text: 'synthetic current secret' });
    const historical = personValue(sensitive, 17, { type: 'text', text: 'synthetic historical secret' }, 0, {
      active_until: '2026-07-01T00:00:00Z', superseded_at: '2026-07-01T00:00:00Z'
    });
    renderSection(vi.fn(), [{ definition: sensitive, current: [current], history: [current, historical] }]);

    expect(screen.getByText('Sensitive')).toBeDefined();
    expect(screen.queryByText('synthetic current secret')).toBeNull();
    expect(screen.queryByText('synthetic historical secret')).toBeNull();
    expect(screen.queryByDisplayValue('synthetic current secret')).toBeNull();
    expect(document.body.innerHTML).not.toContain('synthetic current secret');
    expect(document.body.innerHTML).not.toContain('synthetic historical secret');
    expect(screen.queryByRole('button', { name: /Edit private note/i })).toBeNull();

    const reveal = screen.getByRole('button', { name: 'Reveal Private note values' });
    reveal.focus();
    await fireEvent.click(reveal);
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Hide Private note values' }));
    expect(screen.getByText('synthetic current secret')).toBeDefined();
    expect(screen.getByText('synthetic historical secret')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Edit Private note value 1' }));
    expect(screen.getByRole('textbox', { name: 'Private note' })).toHaveProperty('value', 'synthetic current secret');

    await fireEvent.click(screen.getByRole('button', { name: 'Hide Private note values' }));
    expect(document.body.innerHTML).not.toContain('synthetic current secret');
    expect(document.body.innerHTML).not.toContain('synthetic historical secret');
    expect(screen.queryByRole('textbox', { name: 'Private note' })).toBeNull();
  });

  it.each([
    ['request failure', () => Response.json({ message: 'Synthetic service failure' }, { status: 503 })],
    ['CAS conflict', () => Response.json({
      error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 22
    }, { status: 409 })]
  ])('scrubs a sensitive controller draft after %s when Hide explicitly discards it', async (_name, response) => {
    const sensitive = definition();
    const current = personValue(sensitive, 19, { type: 'text', text: 'server sensitive value' });
    const controller = renderSection(vi.fn<typeof fetch>(async () => response()), [
      { definition: sensitive, current: [current], history: [current] }
    ]);

    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Private note values' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Private note value 1' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Private note' }), { target: { value: 'failed local sensitive draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await screen.findByRole('alert');

    expect(JSON.stringify(controller.draft)).toContain('failed local sensitive draft');
    await fireEvent.click(screen.getByRole('button', { name: 'Hide Private note values' }));

    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();
    expect(document.body.innerHTML).not.toContain('failed local sensitive draft');

    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Private note values' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Private note value 1' }));
    expect(screen.getByRole('textbox', { name: 'Private note' })).toHaveProperty('value', 'server sensitive value');
    expect(document.body.innerHTML).not.toContain('failed local sensitive draft');
  });

  it.each([
    ['request failure', () => Response.json({ message: 'Deferred service failure' }, { status: 503 })],
    ['CAS conflict', () => Response.json({
      error: 'attribute_value_conflict', message: 'deferred conflict', current_value_id: 23
    }, { status: 409 })]
  ])('keeps sensitive plaintext discarded when Hide occurs before a deferred save %s settles', async (_name, response) => {
    let settle: ((response: Response) => void) | undefined;
    const pendingResponse = new Promise<Response>((resolve) => { settle = resolve; });
    const sensitive = definition();
    const current = personValue(sensitive, 19, { type: 'text', text: 'server sensitive value' });
    const controller = renderSection(vi.fn<typeof fetch>(async () => pendingResponse), [
      { definition: sensitive, current: [current], history: [current] }
    ]);

    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Private note values' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Private note value 1' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Private note' }), { target: { value: 'in-flight sensitive draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(controller.mutationPending).toBe(true));

    await fireEvent.click(screen.getByRole('button', { name: 'Hide Private note values' }));
    settle?.(response());
    await waitFor(() => expect(controller.mutationPending).toBe(false));

    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();
    expect(document.body.innerHTML).not.toContain('in-flight sensitive draft');
    expect(screen.queryByRole('textbox', { name: 'Private note' })).toBeNull();
  });

  it('keeps sensitive plaintext discarded when Hide occurs during a deferred failed reload', async () => {
    const reloadSettlers: Array<(response: Response) => void> = [];
    const sensitive = definition();
    const current = personValue(sensitive, 19, { type: 'text', text: 'server sensitive value' });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'PUT') return Response.json({
        error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 22
      }, { status: 409 });
      return new Promise<Response>((resolve) => { reloadSettlers.push(resolve); });
    });
    const controller = renderSection(fetchFn, [
      { definition: sensitive, current: [current], history: [current] }
    ]);

    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Private note values' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Edit Private note value 1' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Private note' }), { target: { value: 'reload-sensitive draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await screen.findByRole('alert');
    await fireEvent.click(screen.getByRole('button', { name: 'Reload attributes' }));
    await waitFor(() => expect(controller.reloadPending).toBe(true));
    await waitFor(() => expect(reloadSettlers).toHaveLength(4));

    await fireEvent.click(screen.getByRole('button', { name: 'Hide Private note values' }));
    for (const settle of reloadSettlers) settle(Response.json({ message: 'Deferred reload failure' }, { status: 503 }));
    await waitFor(() => expect(controller.reloadPending).toBe(false));

    expect(controller.draft).toBeNull();
    expect(controller.conflict).toBeNull();
    expect(document.body.innerHTML).not.toContain('reload-sensitive draft');
    expect(screen.queryByRole('textbox', { name: 'Private note' })).toBeNull();
  });

  it('requires reveal and deliberate selection before adding a sensitive choice', async () => {
    const requests: Request[] = [];
    const sensitiveChoice = definition({
      universal_id: 'sensitive-choice-id', slug: 'confidential_level', label: 'Confidential level',
      field_type: 'select', options: { choices: [{ value: 'private', label: 'Private' }, { value: 'restricted', label: 'Restricted' }] }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json({ dry_run: false, value: personValue(sensitiveChoice, 71, body.value) });
    });
    renderSection(fetchFn, [{ definition: sensitiveChoice, current: [], history: [] }]);

    expect(screen.getByRole('button', { name: 'Add Confidential level value' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Reveal Confidential level values' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Add Confidential level value' }));

    const select = screen.getByRole('combobox', { name: 'Confidential level' });
    expect(select).toHaveProperty('value', '');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('alert').textContent).toContain('Choose an allowed value.');

    await fireEvent.change(select, { target: { value: 'restricted' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'restricted' }, source: 'user'
    });
  });

  it('joins registry definitions to grouped values by universal ID and renders definition metadata plus collapsed history', () => {
    const stale = definition({
      universal_id: 'portable-field-id', slug: 'portable_field', label: 'Stale label', is_sensitive: false
    });
    const registry = definition({
      universal_id: 'portable-field-id', slug: 'portable_field', label: 'Portable field', description: 'Registry description',
      is_sensitive: false, cardinality: 'multi', options: { choices: [{ value: 'one', label: 'One' }, { value: 'two', label: 'Two' }] }
    });
    const current = personValue(stale, 31, { type: 'text', text: 'one' }, 2);
    const historical = personValue(stale, 29, { type: 'text', text: 'two' }, 2, {
      source: 'vcard_import', source_ref: 'synthetic-source', active_until: when, superseded_at: when
    });

    renderSection(vi.fn(), [{ definition: stale, current: [current], history: [current, historical] }], [registry]);

    expect(screen.getByRole('heading', { name: 'Portable field' })).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Stale label' })).toBeNull();
    expect(screen.getByText('Registry description')).toBeDefined();
    expect(screen.getByText(/Type: text/)).toBeDefined();
    expect(screen.getByText(/Cardinality: multi/)).toBeDefined();
    expect(screen.getByText(/Ownership: user/)).toBeDefined();
    expect(screen.getByText('Allowed choices: One, Two')).toBeDefined();
    const history = screen.getByText('History (1)').closest('details');
    expect(history).not.toBeNull();
    expect(history?.hasAttribute('open')).toBe(false);
    expect(screen.getByText(/Source: vcard_import/)).toBeDefined();
    expect(screen.getByText(/Reference: synthetic-source/)).toBeDefined();
  });

  it('confirms and clears the selected multi-value with its exact CAS identity', async () => {
    const requests: Request[] = [];
    const multi = definition({
      universal_id: 'multi-field-id', slug: 'topics', label: 'Topics', is_sensitive: false, cardinality: 'multi', field_type: 'text'
    });
    const current = personValue(multi, 41, { type: 'text', text: 'Databases' }, 3);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json({ dry_run: false, superseded: { ...current, active_until: when, superseded_at: when } });
    });
    renderSection(fetchFn, [{ definition: multi, current: [current], history: [current] }]);

    await fireEvent.click(screen.getByRole('button', { name: 'Close Topics value 1' }));
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('group', { name: 'Confirm closing Topics value 1' })).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Confirm close attribute' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    const url = new URL(requests[0]!.url);
    expect(requests[0]!.method).toBe('DELETE');
    expect(url.pathname).toBe('/api/v1/people/7/attributes/topics');
    expect([...url.searchParams.entries()]).toEqual([['expected_value_id', '41'], ['ordinal', '3']]);
    await waitFor(() => expect(screen.getByText('No current value.')).toBeDefined());
    expect(screen.getByText('Databases')).toBeDefined();
    expect(screen.getByText('History (1)')).toBeDefined();
  });

  it('keeps the selected clear target and reports a retryable request failure', async () => {
    const note = definition({ is_sensitive: false });
    const current = personValue(note, 42, { type: 'text', text: 'Keep this draft' });
    renderSection(vi.fn<typeof fetch>(async () => Response.json({ message: 'Synthetic service failure' }, { status: 503 })), [
      { definition: note, current: [current], history: [current] }
    ]);

    await fireEvent.click(screen.getByRole('button', { name: 'Close Private note value 1' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Confirm close attribute' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic service failure');
    expect(screen.getByRole('group', { name: 'Confirm closing Private note value 1' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Confirm close attribute' })).toHaveProperty('disabled', false);
    expect(screen.queryByRole('button', { name: 'Reload attributes' })).toBeNull();
  });

  it('keeps the conflicted editor mounted and gates opening another attribute', async () => {
    const requests: Request[] = [];
    const alias = definition({
      universal_id: 'alias-id', slug: 'alias', label: 'Alias', is_sensitive: false, field_type: 'text'
    });
    const nickname = definition({
      id: 9, universal_id: 'nickname-id', slug: 'nickname', label: 'Nickname', is_sensitive: false,
      field_type: 'text', display_order: 20
    });
    const current = personValue(alias, 43, { type: 'text', text: 'Original alias' });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      requests.push(input instanceof Request ? input : new Request(input));
      return Response.json({ error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 44 }, { status: 409 });
    });
    const controller = renderSection(fetchFn, [
      { definition: alias, current: [current], history: [current] },
      { definition: nickname, current: [], history: [] }
    ]);

    await fireEvent.click(screen.getByRole('button', { name: 'Edit Alias value 1' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Alias' }), { target: { value: 'Retained local alias' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect((await screen.findByRole('alert')).textContent).toContain('This person changed elsewhere.');

    const retainedDraft = controller.draft;
    expect(screen.getByRole('button', { name: 'Add Nickname value' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Add Nickname value' }));

    expect(requests).toHaveLength(1);
    expect(controller.draft).toBe(retainedDraft);
    expect(screen.getByRole('textbox', { name: 'Alias' })).toHaveProperty('value', 'Retained local alias');
    expect(screen.queryByRole('textbox', { name: 'Nickname' })).toBeNull();
  });

  it('lets the server allocate a new multi-value ordinal and disables operations the definition does not permit', async () => {
    const requests: Request[] = [];
    const multi = definition({
      universal_id: 'multi-field-id', slug: 'topics', label: 'Topics', is_sensitive: false, cardinality: 'multi', field_type: 'text'
    });
    const derived = definition({
      id: 9, universal_id: 'derived-field-id', slug: 'last_contacted', label: 'Last contacted', is_sensitive: false,
      value_type: 'timestamp', field_type: 'timestamp', ownership: 'system', ui_creatable: false, ui_editable: false,
      api_mutable: false, derived_source: 'activity_spine', history_exempt: true, display_order: 20
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json({ dry_run: false, value: personValue(multi, 50, body.value, 5) });
    });
    renderSection(fetchFn, [
      { definition: multi, current: [], history: [] },
      { definition: derived, current: [personValue(derived, 49, { type: 'timestamp', timestamp: when })], history: [] }
    ]);

    await fireEvent.click(screen.getByRole('button', { name: 'Add Topics value' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Topics' }), { target: { value: 'Search systems' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'Search systems' }, source: 'user'
    });

    expect(screen.getByRole('button', { name: 'Add Last contacted value' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Edit Last contacted value 1' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Close Last contacted value 1' })).toHaveProperty('disabled', true);
    expect(screen.getByText(/Computed by activity_spine/)).toBeDefined();
    expect(screen.queryByRole('button', { name: /rename|delete.*definition/i })).toBeNull();
  });

  it('opens user-writable timestamp and JSON definitions from the joined section', async () => {
    const timestamp = definition({
      id: 10, universal_id: 'timestamp-id', slug: 'follow_up_at', label: 'Follow up at', is_sensitive: false,
      value_type: 'timestamp', field_type: 'timestamp', options: undefined
    });
    const json = definition({
      id: 11, universal_id: 'json-id', slug: 'preferences', label: 'Preferences', is_sensitive: false,
      value_type: 'json', field_type: 'textarea', options: undefined, display_order: 20
    });
    renderSection(vi.fn(), [
      { definition: timestamp, current: [], history: [] },
      { definition: json, current: [], history: [] }
    ]);

    const timestampAdd = screen.getByRole('button', { name: 'Add Follow up at value' });
    expect(timestampAdd).toHaveProperty('disabled', false);
    await fireEvent.click(timestampAdd);
    expect(screen.getByRole('textbox', { name: 'Follow up at' })).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    const jsonAdd = screen.getByRole('button', { name: 'Add Preferences value' });
    expect(jsonAdd).toHaveProperty('disabled', false);
    await fireEvent.click(jsonAdd);
    expect(screen.getByRole('textbox', { name: 'Preferences' })).toBeDefined();
  });
});
