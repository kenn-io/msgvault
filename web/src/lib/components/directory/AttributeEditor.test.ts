import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import AttributeEditor from './AttributeEditor.svelte';

type AttributeDefinition = components['schemas']['AttributeDefinition'];
type AttributeValue = components['schemas']['AttributeValue'];
type PersonAttributeValue = components['schemas']['PersonAttributeValue'];

const when = '2026-08-01T00:00:00Z';
const currentMonthDate = (() => {
  const now = new Date();
  return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-17`;
})();
const currentMonthDateLabel = new Intl.DateTimeFormat(undefined, {
  year: 'numeric', month: 'short', day: 'numeric'
}).format(new Date(`${currentMonthDate}T00:00:00`));

afterEach(() => cleanup());

function definition(overrides: Partial<AttributeDefinition> = {}): AttributeDefinition {
  return {
    id: 4,
    universal_id: '00000000-0000-4000-8000-000000000004',
    object_type: 'person',
    slug: 'relationship_status',
    label: 'Relationship status',
    description: 'How this person is known',
    value_type: 'text',
    field_type: 'select',
    cardinality: 'single',
    display_order: 10,
    is_required: false,
    ownership: 'user',
    ui_creatable: true,
    ui_editable: true,
    api_mutable: true,
    is_searchable: true,
    is_sensitive: false,
    is_audited: true,
    is_deletable: true,
    history_exempt: false,
    is_active: true,
    revision: 1,
    created_at: when,
    updated_at: when,
    options: { choices: [{ value: 'friend', label: 'Friend' }, { value: 'colleague', label: 'Colleague' }] },
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

function controllerFor(
  fetchFn: typeof fetch,
  definitionValue: AttributeDefinition,
  current: PersonAttributeValue[] = []
): DirectoryProfileController {
  const bundle = {
    attributes: {
      person_id: 7,
      attributes: [{ definition: definitionValue, current, history: [...current] }]
    },
    definitions: { definitions: [definitionValue] },
    etags: {},
    errors: {}
  } satisfies DirectoryReadBundle;
  return new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle);
}

function attributeWrite(body: components['schemas']['SetPersonAttributeRequest'], definitionValue: AttributeDefinition, id = 90) {
  return {
    dry_run: false,
    value: personValue(definitionValue, id, body.value, body.ordinal ?? 0),
    ...(body.expected_value_id
      ? { superseded: personValue(definitionValue, body.expected_value_id, { type: definitionValue.value_type, text: 'Old value' }, body.ordinal ?? 0, { active_until: when, superseded_at: when }) }
      : {})
  };
}

describe('AttributeEditor', () => {
  it('uses a constrained choice and the selected single value ID without inventing an ordinal', async () => {
    const requests: Request[] = [];
    const choice = definition();
    const current = personValue(choice, 19, { type: 'text', text: 'colleague' });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, choice));
    });
    const controller = controllerFor(fetchFn, choice, [current]);

    render(AttributeEditor, { controller, definition: choice, current });
    await fireEvent.change(screen.getByLabelText('Relationship status'), { target: { value: 'friend' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/people/7/attributes/relationship_status');
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'friend' },
      expected_value_id: 19,
      source: 'user'
    });
  });

  it.each([
    {
      name: 'boolean',
      definition: definition({ slug: 'subscribed', universal_id: 'boolean-id', label: 'Subscribed', value_type: 'boolean', field_type: 'checkbox', options: undefined }),
      input: async () => fireEvent.click(screen.getByRole('checkbox', { name: 'Subscribed' })),
      want: { type: 'boolean', boolean: true }
    },
    {
      name: 'integer',
      definition: definition({ slug: 'contact_frequency', universal_id: 'integer-id', label: 'Contact frequency', value_type: 'integer', field_type: 'duration', options: { unit: 'days' } }),
      input: async () => fireEvent.input(screen.getByLabelText('Contact frequency'), { target: { value: '14' } }),
      want: { type: 'integer', integer: 14 }
    },
    {
      name: 'real number',
      definition: definition({ slug: 'rating', universal_id: 'real-id', label: 'Rating', value_type: 'real', field_type: 'text', options: undefined }),
      input: async () => fireEvent.input(screen.getByLabelText('Rating'), { target: { value: '4.25' } }),
      want: { type: 'real', real: 4.25 }
    },
    {
      name: 'date',
      definition: definition({ slug: 'met_on', universal_id: 'date-id', label: 'Met on', value_type: 'date', field_type: 'date', options: undefined }),
      input: async () => fireEvent.click(screen.getByRole('button', { name: currentMonthDateLabel })),
      want: { type: 'date', date: currentMonthDate }
    },
    {
      name: 'person reference',
      definition: definition({ slug: 'introduced_by', universal_id: 'person-id', label: 'Introduced by', value_type: 'record_reference', field_type: 'person', record_target: 'person', options: undefined }),
      input: async () => fireEvent.input(screen.getByLabelText('Introduced by'), { target: { value: '42' } }),
      want: { type: 'record_reference', record_type: 'person', record_id: 42 }
    },
    {
      name: 'text',
      definition: definition({ slug: 'nickname', universal_id: 'text-id', label: 'Nickname', value_type: 'text', field_type: 'text', options: undefined }),
      input: async () => fireEvent.input(screen.getByLabelText('Nickname'), { target: { value: 'Synthetic nickname' } }),
      want: { type: 'text', text: 'Synthetic nickname' }
    }
  ])('builds the generated $name value union without CAS for a new value', async ({ definition: definitionValue, input, want }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (requestInput) => {
      const request = requestInput instanceof Request ? requestInput : new Request(requestInput);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, definitionValue));
    });
    const controller = controllerFor(fetchFn, definitionValue);

    render(AttributeEditor, { controller, definition: definitionValue });
    await input();
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({ value: want, source: 'user' });
  });

  it('replaces the selected multi-value lineage with its exact value ID and ordinal', async () => {
    const requests: Request[] = [];
    const multi = definition({
      slug: 'ask_me_about', universal_id: 'multi-id', label: 'Ask me about', value_type: 'text', field_type: 'text', cardinality: 'multi', options: undefined
    });
    const current = personValue(multi, 33, { type: 'text', text: 'Old topic' }, 4);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, multi));
    });
    const controller = controllerFor(fetchFn, multi, [current]);

    render(AttributeEditor, { controller, definition: multi, current });
    await fireEvent.input(screen.getByLabelText('Ask me about'), { target: { value: 'New topic' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'New topic' }, expected_value_id: 33, ordinal: 4, source: 'user'
    });
  });

  it.each([
    {
      name: 'timestamp',
      definition: definition({
        slug: 'follow_up_at', universal_id: 'timestamp-id', label: 'Follow up at', value_type: 'timestamp', field_type: 'timestamp', options: undefined
      }),
      draft: '2024-02-29T23:59:59.123456789+05:30',
      want: { type: 'timestamp', timestamp: '2024-02-29T23:59:59.123456789+05:30' }
    },
    {
      name: 'JSON',
      definition: definition({
        slug: 'preferences', universal_id: 'json-id', label: 'Preferences', value_type: 'json', field_type: 'textarea', options: undefined
      }),
      draft: '{"theme":"dark","alerts":true}',
      want: { type: 'json', json: { theme: 'dark', alerts: true } }
    }
  ])('emits the exact generated $name union for a valid definition-driven draft', async ({ definition: definitionValue, draft, want }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, definitionValue));
    });
    const controller = controllerFor(fetchFn, definitionValue);

    render(AttributeEditor, { controller, definition: definitionValue });
    await fireEvent.input(screen.getByRole('textbox', { name: definitionValue.label }), { target: { value: draft } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({ value: want, source: 'user' });
  });

  it.each([
    ['timestamp', definition({ slug: 'follow_up_at', label: 'Follow up at', value_type: 'timestamp', field_type: 'timestamp', options: undefined }), 'tomorrow', 'Enter an RFC3339 timestamp.'],
    ['normalized invalid date', definition({ slug: 'follow_up_at', label: 'Follow up at', value_type: 'timestamp', field_type: 'timestamp', options: undefined }), '2026-02-30T14:30:45Z', 'Enter an RFC3339 timestamp.'],
    ['non-leap February date', definition({ slug: 'follow_up_at', label: 'Follow up at', value_type: 'timestamp', field_type: 'timestamp', options: undefined }), '2025-02-29T14:30:45Z', 'Enter an RFC3339 timestamp.'],
    ['normalized 24-hour time', definition({ slug: 'follow_up_at', label: 'Follow up at', value_type: 'timestamp', field_type: 'timestamp', options: undefined }), '2026-01-01T24:00:00Z', 'Enter an RFC3339 timestamp.'],
    ['out-of-range offset', definition({ slug: 'follow_up_at', label: 'Follow up at', value_type: 'timestamp', field_type: 'timestamp', options: undefined }), '2026-01-01T12:00:00+24:00', 'Enter an RFC3339 timestamp.'],
    ['JSON syntax', definition({ slug: 'preferences', label: 'Preferences', value_type: 'json', field_type: 'textarea', options: undefined }), '{bad', 'Enter valid JSON.'],
    ['null JSON', definition({ slug: 'preferences', label: 'Preferences', value_type: 'json', field_type: 'textarea', options: undefined }), 'null', 'JSON cannot be null.']
  ])('blocks a malformed $name draft before issuing a request', async (_name, definitionValue, draft, message) => {
    const fetchFn = vi.fn<typeof fetch>();
    const controller = controllerFor(fetchFn, definitionValue);

    render(AttributeEditor, { controller, definition: definitionValue });
    await fireEvent.input(screen.getByRole('textbox', { name: definitionValue.label }), { target: { value: draft } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    expect(fetchFn).not.toHaveBeenCalled();
    expect(screen.getByRole('alert').textContent).toContain(message);
  });

  it('displays a live store-aligned count and enforces text max_length before issuing a request', async () => {
    const requests: Request[] = [];
    const shortNote = definition({
      slug: 'short_note', label: 'Short note', field_type: 'text', options: { max_length: 5 }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, shortNote));
    });
    const controller = controllerFor(fetchFn, shortNote);

    render(AttributeEditor, { controller, definition: shortNote });
    const input = screen.getByRole('textbox', { name: 'Short note' });
    const constraintID = input.getAttribute('aria-describedby');
    expect(constraintID).not.toBeNull();
    expect(document.getElementById(constraintID!)?.textContent).toBe('0 / 5 characters.');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Short note' }), { target: { value: '123456' } });
    expect(document.getElementById(constraintID!)?.textContent).toBe('6 / 5 characters.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    expect(requests).toHaveLength(0);
    expect(screen.getByRole('alert').textContent).toContain('Use 5 characters or fewer.');

    await fireEvent.input(screen.getByRole('textbox', { name: 'Short note' }), { target: { value: '12345' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: '12345' }, source: 'user'
    });
  });

  it('lets a textarea enter trimmed astral characters up to the store rune limit', async () => {
    const requests: Request[] = [];
    const shortNote = definition({
      slug: 'short_note', label: 'Short note', field_type: 'textarea', options: { max_length: 5 }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, shortNote));
    });
    const controller = controllerFor(fetchFn, shortNote);

    render(AttributeEditor, { controller, definition: shortNote });
    const textarea = screen.getByRole('textbox', { name: 'Short note' });
    expect(textarea.getAttribute('maxlength')).toBeNull();
    await fireEvent.input(textarea, { target: { value: '  😀😀😀😀😀😀  ' } });
    expect(textarea).toHaveProperty('value', '  😀😀😀😀😀😀  ');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('alert').textContent).toContain('Use 5 characters or fewer.');

    await fireEvent.input(textarea, { target: { value: '  😀😀😀😀😀  ' } });
    const constraintID = textarea.getAttribute('aria-describedby');
    expect(document.getElementById(constraintID!)?.textContent).toBe('5 / 5 characters.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: '😀😀😀😀😀' }, source: 'user'
    });
  });

  it('trims U+0085 NEL like Go for the visible count and exact text request', async () => {
    const requests: Request[] = [];
    const shortNote = definition({
      slug: 'short_note', label: 'Short note', field_type: 'text', options: { max_length: 1 }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, shortNote));
    });
    const controller = controllerFor(fetchFn, shortNote);

    render(AttributeEditor, { controller, definition: shortNote });
    const input = screen.getByRole('textbox', { name: 'Short note' });
    await fireEvent.input(input, { target: { value: '\u0085a\u0085' } });
    const constraintID = input.getAttribute('aria-describedby');
    expect(document.getElementById(constraintID!)?.textContent).toBe('1 / 1 characters.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'a' }, source: 'user'
    });
  });

  it('preserves and counts U+FEFF BOM like Go while enforcing the rune limit', async () => {
    const requests: Request[] = [];
    const shortNote = definition({
      slug: 'short_note', label: 'Short note', field_type: 'text', options: { max_length: 2 }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, shortNote));
    });
    const controller = controllerFor(fetchFn, shortNote);

    render(AttributeEditor, { controller, definition: shortNote });
    const input = screen.getByRole('textbox', { name: 'Short note' });
    const constraintID = input.getAttribute('aria-describedby');
    await fireEvent.input(input, { target: { value: '\uFEFFa\uFEFF' } });
    expect(document.getElementById(constraintID!)?.textContent).toBe('3 / 2 characters.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('alert').textContent).toContain('Use 2 characters or fewer.');

    await fireEvent.input(input, { target: { value: '\uFEFFa' } });
    expect(document.getElementById(constraintID!)?.textContent).toBe('2 / 2 characters.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: '\uFEFFa' }, source: 'user'
    });
  });

  it('associates a text-choice max length and uses store-aligned code-point counts', async () => {
    const requests: Request[] = [];
    const choice = definition({
      slug: 'symbol_choice', label: 'Symbol choice', field_type: 'select',
      options: { max_length: 5, choices: [
        { value: '😀😀😀😀😀', label: 'Five symbols' },
        { value: '😀😀😀😀😀😀', label: 'Six symbols' },
        { value: '\uFEFF12345', label: 'BOM plus five characters' }
      ] }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json(attributeWrite(body, choice));
    });
    const controller = controllerFor(fetchFn, choice);

    render(AttributeEditor, { controller, definition: choice });
    const select = screen.getByRole('combobox', { name: 'Symbol choice' });
    const constraintID = select.getAttribute('aria-describedby');
    expect(constraintID).not.toBeNull();
    expect(document.getElementById(constraintID!)?.textContent).toBe('5 / 5 characters.');

    await fireEvent.change(select, { target: { value: '😀😀😀😀😀😀' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect(requests).toHaveLength(0);
    expect(screen.getByRole('alert').textContent).toContain('Use 5 characters or fewer.');

    await fireEvent.change(select, { target: { value: '\uFEFF12345' } });
    expect(document.getElementById(constraintID!)?.textContent).toBe('6 / 5 characters.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    expect(requests).toHaveLength(0);

    await fireEvent.change(select, { target: { value: '😀😀😀😀😀' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(requests).toHaveLength(1));
  });

  it('rebases a conflicted multi edit to the fresh current ID at the same ordinal', async () => {
    const requests: Request[] = [];
    let setAttempts = 0;
    const note = definition({
      slug: 'note', universal_id: 'note-id', label: 'Note', field_type: 'text', cardinality: 'multi', options: undefined
    });
    const original = personValue(note, 19, { type: 'text', text: 'Original' }, 4);
    const server = personValue(note, 21, { type: 'text', text: 'Server value' }, 4);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const url = new URL(request.url);
      if (request.method === 'PUT') {
        setAttempts += 1;
        if (setAttempts === 1) return Response.json({
          error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 21, current_value: server
        }, { status: 409 });
        const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
        return Response.json(attributeWrite(body, note, 22));
      }
      if (url.pathname.endsWith('/attributes')) return Response.json({
        person_id: 7, attributes: [{ definition: note, current: [server], history: [server] }]
      });
      if (url.pathname === '/api/v1/attribute-definitions') return Response.json({ definitions: [note] });
      if (url.pathname.endsWith('/profile')) return new Response(JSON.stringify({ person: { id: 7 } }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' } });
      return new Response(JSON.stringify({ id: 7 }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' } });
    });
    const controller = controllerFor(fetchFn, note, [original]);

    render(AttributeEditor, { controller, definition: note, current: original });
    await fireEvent.input(screen.getByLabelText('Note'), { target: { value: 'Local draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    expect((await screen.findByRole('alert')).textContent).toContain('This person changed elsewhere. Reload and retry.');
    expect(screen.getByLabelText('Note')).toHaveProperty('value', 'Local draft');
    expect(screen.getByRole('button', { name: 'Save attribute' })).toHaveProperty('disabled', true);

    await fireEvent.click(screen.getByRole('button', { name: 'Reload attributes' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Save attribute' })).toHaveProperty('disabled', false));
    expect(screen.getByLabelText('Note')).toHaveProperty('value', 'Local draft');

    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(setAttempts).toBe(2));
    const setRequests = requests.filter((request) => request.method === 'PUT');
    await expect(setRequests[1]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'Local draft' }, expected_value_id: 21, ordinal: 4, source: 'user'
    });
  });

  it('requires deliberate conversion to a new add when a conflicted multi lineage disappeared', async () => {
    const requests: Request[] = [];
    let setAttempts = 0;
    const note = definition({
      slug: 'note', universal_id: 'note-id', label: 'Note', field_type: 'text', cardinality: 'multi', options: undefined
    });
    const original = personValue(note, 19, { type: 'text', text: 'Original' }, 4);
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const url = new URL(request.url);
      if (request.method === 'PUT') {
        setAttempts += 1;
        if (setAttempts === 1) return Response.json({
          error: 'attribute_value_conflict', message: 'changed elsewhere', current_value_id: 19
        }, { status: 409 });
        const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
        return Response.json(attributeWrite(body, note, 22));
      }
      if (url.pathname.endsWith('/attributes')) return Response.json({
        person_id: 7, attributes: [{ definition: note, current: [], history: [
          personValue(note, 19, { type: 'text', text: 'Original' }, 4, { active_until: when, superseded_at: when })
        ] }]
      });
      if (url.pathname === '/api/v1/attribute-definitions') return Response.json({ definitions: [note] });
      if (url.pathname.endsWith('/profile')) return new Response(JSON.stringify({ person: { id: 7 } }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' } });
      return new Response(JSON.stringify({ id: 7 }), { headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' } });
    });
    const controller = controllerFor(fetchFn, note, [original]);

    render(AttributeEditor, { controller, definition: note, current: original });
    await fireEvent.input(screen.getByLabelText('Note'), { target: { value: 'Local draft' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Reload attributes' }));

    expect(await screen.findByText('The selected value is no longer current. Cancel or add this draft as a new value.')).toBeDefined();
    expect(screen.getByLabelText('Note')).toHaveProperty('value', 'Local draft');
    expect(screen.getByRole('button', { name: 'Save attribute' })).toHaveProperty('disabled', true);
    expect(setAttempts).toBe(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Add draft as new value' }));
    expect(screen.getByRole('form', { name: 'Add Note value' })).toBeDefined();
    expect(screen.queryByRole('form', { name: 'Edit Note value' })).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('This draft will be added as a new value.');
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

    await waitFor(() => expect(setAttempts).toBe(2));
    const setRequests = requests.filter((request) => request.method === 'PUT');
    await expect(setRequests[1]!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: 'Local draft' }, source: 'user'
    });
  });
});
