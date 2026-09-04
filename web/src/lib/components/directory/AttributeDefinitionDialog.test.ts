import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';
import AttributeDefinitionDialog from './AttributeDefinitionDialog.svelte';
import AttributeSection from './AttributeSection.svelte';

type AttributeDefinition = components['schemas']['AttributeDefinition'];

const when = '2026-08-01T00:00:00Z';

afterEach(() => cleanup());

function definition(overrides: Partial<AttributeDefinition> = {}): AttributeDefinition {
  return {
    id: 41,
    universal_id: '00000000-0000-4000-8000-000000000041',
    object_type: 'person',
    slug: 'preferred_channel',
    label: 'Preferred channel',
    description: 'How this person prefers to be contacted',
    value_type: 'text',
    field_type: 'select',
    cardinality: 'single',
    display_order: 0,
    is_required: false,
    ownership: 'user',
    ui_creatable: true,
    ui_editable: true,
    api_mutable: true,
    is_searchable: false,
    is_sensitive: true,
    is_audited: false,
    is_deletable: true,
    history_exempt: false,
    options: { choices: [{ value: 'email', label: 'email' }, { value: 'phone', label: 'phone' }] },
    is_active: true,
    revision: 1,
    created_at: when,
    updated_at: when,
    ...overrides
  };
}

function controller(fetchFn: typeof fetch, definitions: AttributeDefinition[] = []): DirectoryProfileController {
  const bundle = {
    definitions: { definitions },
    etags: {},
    errors: {}
  } satisfies DirectoryReadBundle;
  return new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle);
}

describe('AttributeDefinitionDialog', () => {
  it('creates a sensitive user choice definition, refreshes the registry, and focuses the returned identity', async () => {
    const requests: Request[] = [];
    const created = definition();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      return Response.json({ definitions: [created] });
    });
    const profile = controller(fetchFn);

    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: ' Preferred channel ' } });
    await fireEvent.input(screen.getByLabelText('Description'), { target: { value: ' How this person prefers to be contacted ' } });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: ' email\nphone ' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Sensitive' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    await waitFor(() => expect(profile.createdDefinition).toEqual(created));
    expect(requests).toHaveLength(2);
    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', '/api/v1/attribute-definitions'],
      ['GET', '/api/v1/attribute-definitions']
    ]);
    const body = await requests[0]!.clone().json() as Record<string, unknown>;
    expect(body).toEqual({
      object_type: 'person',
      label: 'Preferred channel',
      description: 'How this person prefers to be contacted',
      value_type: 'text',
      field_type: 'select',
      cardinality: 'single',
      is_sensitive: true,
      options: { choices: [{ value: 'email', label: 'email' }, { value: 'phone', label: 'phone' }] }
    });
    expect(body).not.toHaveProperty('ownership');
    expect(profile.createdDefinition).toEqual(created);
    expect(profile.definitions).toEqual([created]);
    expect(profile.createdDefinition).toBe(profile.definitions[0]);
    const result = screen.getByRole('status');
    expect(result.textContent).toContain('preferred_channel');
    expect(result.textContent).toContain(created.universal_id);
    expect(document.activeElement).toBe(result);
  });

  it.each([
    ['Text', 'text', 'text', undefined],
    ['Integer', 'integer', 'duration', undefined],
    ['Number', 'real', 'text', undefined],
    ['Boolean', 'boolean', 'checkbox', undefined],
    ['Date', 'date', 'date', undefined],
    ['Timestamp', 'timestamp', 'timestamp', undefined],
    ['JSON', 'json', 'json', undefined],
    ['Person reference', 'record_reference', 'person', 'person']
  ])('sends a server-compatible %s definition', async (optionLabel, valueType, fieldType, recordTarget) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const created = definition({
        label: 'Synthetic field', slug: 'synthetic_field', value_type: valueType,
        field_type: fieldType, ...(recordTarget ? { record_target: recordTarget } : {}), is_sensitive: false,
        options: undefined
      });
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      return Response.json({ definitions: [created] });
    });

    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Synthetic field' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), optionLabel);
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));

    const body = await requests[0]!.clone().json() as Record<string, unknown>;
    expect(body).toMatchObject({ value_type: valueType, field_type: fieldType });
    if (recordTarget) expect(body).toHaveProperty('record_target', recordTarget);
    else expect(body).not.toHaveProperty('record_target');
  });

  it('sends trimmed labeled text choices and max length with multi-select cardinality', async () => {
    const requests: Request[] = [];
    const created = definition({ cardinality: 'multi', field_type: 'multiselect', is_sensitive: false });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST'
        ? Response.json(created, { status: 201 })
        : Response.json({ definitions: [created] });
    });

    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Preferred channel' } });
    await chooseSelectOption(screen.getByLabelText('Cardinality'), 'Multiple values');
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: ' email | Email\n phone | Phone ' } });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: ' 32 ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));

    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      cardinality: 'multi', field_type: 'multiselect',
      options: {
        choices: [{ value: 'email', label: 'Email' }, { value: 'phone', label: 'Phone' }],
        max_length: 32
      }
    });
  });

  it.each([
    ['a blank label', '', '', 'Enter a label.'],
    ['a choice missing its value', 'Field', ' | Visible label', 'Each choice needs a value.'],
    ['a choice missing its label', 'Field', 'value | ', 'Each choice needs a label.'],
    ['trim-equivalent choices', 'Field', ' email | Email\nemail | Other', 'Choice values must be unique.']
  ])('reports %s before issuing a request', async (_case, label, choices, message) => {
    const fetchFn = vi.fn<typeof fetch>();
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    if (label) await fireEvent.input(screen.getByLabelText('Label'), { target: { value: label } });
    if (choices) await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: choices } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect(screen.getByRole('alert').textContent).toContain(message);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('uses the server canonical value when detecting duplicate integer choices', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Integer choice' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), 'Integer');
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: '01 | First\n1 | Second' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect(screen.getByRole('alert').textContent).toContain('Choice values must be unique.');
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('clears and hides choices when switching to a value type without a server canonical string', async () => {
    const requests: Request[] = [];
    const created = definition({ value_type: 'json', field_type: 'json', options: undefined, is_sensitive: false });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST' ? Response.json(created, { status: 201 }) : Response.json({ definitions: [created] });
    });
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'JSON field' } });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: 'stale choice' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), 'JSON');
    expect(screen.queryByLabelText('Choices')).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    const body = await requests[0]!.clone().json() as Record<string, unknown>;
    expect(body).not.toHaveProperty('options');
  });

  it.each([
    ['-1', 'Maximum length must be a positive whole number'],
    ['1.5', 'Maximum length must be a positive whole number']
  ])('rejects invalid text maximum length %s before issuing a request', async (limit, message) => {
    const fetchFn = vi.fn<typeof fetch>();
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Limited text' } });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: limit } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    expect(screen.getByRole('alert').textContent).toContain(message);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('treats zero text maximum length as unset', async () => {
    const requests: Request[] = [];
    const created = definition({ field_type: 'text', options: undefined, is_sensitive: false });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST' ? Response.json(created, { status: 201 }) : Response.json({ definitions: [created] });
    });
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Unlimited text' } });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '0' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    const body = await requests[0]!.clone().json() as Record<string, unknown>;
    expect(body).not.toHaveProperty('options');
  });

  it.each([
    ['ordinary text', 'email', '4'],
    ['surrounding Go space', '  email  ', '4'],
    ['astral code points', '😀😀', '1'],
    ['NEL boundaries', '\u0085ab\u0085', '1'],
    ['BOM content boundaries', '\uFEFFa\uFEFF', '2']
  ])('blocks a text choice over max_length after canonical normalization: %s', async (_case, choice, limit) => {
    const created = definition({ field_type: 'select', is_sensitive: false });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.method === 'POST' ? Response.json(created, { status: 201 }) : Response.json({ definitions: [created] });
    });
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Limited choice' } });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: choice } });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: limit } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect(screen.getByRole('alert').textContent).toContain(`Each text choice must be ${limit} characters or fewer.`);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('revalidates a text-choice length error when max_length or value type changes', async () => {
    const fetchFn = vi.fn<typeof fetch>();
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Limited choice' } });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: 'email' } });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '4' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    expect(screen.getByRole('alert').textContent).toContain('Each text choice must be 4 characters or fewer.');

    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '5' } });
    expect(screen.queryByRole('alert')).toBeNull();

    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '4' } });
    expect(screen.getByRole('alert').textContent).toContain('Each text choice must be 4 characters or fewer.');

    await chooseSelectOption(screen.getByLabelText('Value type'), 'JSON');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('creates, refetches, and saves a canonical text choice exactly at max_length through AttributeEditor', async () => {
    const canonicalChoice = '😀\uFEFF';
    const created = definition({
      id: 74, universal_id: 'created-limited-choice', slug: 'limited_choice', label: 'Limited choice',
      field_type: 'select', is_sensitive: false,
      options: { max_length: 2, choices: [{ value: canonicalChoice, label: 'Astral plus BOM' }] }
    });
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      if (request.method === 'GET') return Response.json({ definitions: [created] });
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json({
        dry_run: false,
        value: {
          id: 104, person_id: 7, definition_id: created.id, definition_slug: created.slug,
          ordinal: 0, value: body.value, active_from: when, created_at: when,
          source: 'user', actor: 'synthetic-user'
        }
      });
    });
    render(AttributeSection, { controller: controller(fetchFn) });
    await fireEvent.click(screen.getByRole('button', { name: 'Create attribute field' }));
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Limited choice' } });
    await fireEvent.input(screen.getByLabelText('Choices'), {
      target: { value: `\u0085${canonicalChoice}\u0085 | Astral plus BOM` }
    });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '2' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await screen.findByRole('status');
    await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Add Limited choice value' }));
    await fireEvent.change(screen.getByRole('combobox', { name: 'Limited choice' }), { target: { value: canonicalChoice } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'PUT')).toHaveLength(1));

    const definitionBody = await requests.find((request) => request.method === 'POST')!.clone().json();
    expect(definitionBody).toMatchObject({
      options: { max_length: 2, choices: [{ value: canonicalChoice, label: 'Astral plus BOM' }] }
    });
    await expect(requests.find((request) => request.method === 'PUT')!.clone().json()).resolves.toEqual({
      value: { type: 'text', text: canonicalChoice }, source: 'user'
    });
  });

  it.each(['single', 'multi'] as const)(
    'renders current and history for a created %s definition from its first save without a refetch',
    async (cardinality) => {
      const created = definition({
        id: cardinality === 'single' ? 81 : 82,
        universal_id: `created-first-${cardinality}`,
        slug: `created_first_${cardinality}`,
        label: `First ${cardinality}`,
        field_type: 'text',
        cardinality,
        is_sensitive: false,
        options: undefined
      });
      const requests: Request[] = [];
      let current: components['schemas']['PersonAttributeValue'] | undefined;
      let nextValueID = 100;
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        requests.push(request);
        if (request.method === 'POST') return Response.json(created, { status: 201 });
        if (request.method === 'GET') return Response.json({ definitions: [created] });
        const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
        const prior = current;
        current = {
          id: nextValueID++, person_id: 7, definition_id: created.id, definition_slug: created.slug,
          ordinal: 0, value: body.value, active_from: when, created_at: when, source: 'user', actor: 'synthetic-user'
        };
        return Response.json({
          dry_run: false,
          value: current,
          ...(prior ? { superseded: { ...prior, active_until: when, superseded_at: when } } : {})
        });
      });
      const profile = controller(fetchFn);
      profile.attributes = { person_id: 7, attributes: [] };
      render(AttributeSection, { controller: profile });

      await fireEvent.click(screen.getByRole('button', { name: 'Create attribute field' }));
      await fireEvent.input(screen.getByLabelText('Label'), { target: { value: `First ${cardinality}` } });
      if (cardinality === 'multi') await chooseSelectOption(screen.getByLabelText('Cardinality'), 'Multiple values');
      await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
      await screen.findByRole('status');
      await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
      await fireEvent.click(screen.getByRole('button', { name: `Add First ${cardinality} value` }));
      await fireEvent.input(screen.getByRole('textbox', { name: `First ${cardinality}` }), { target: { value: 'Initial value' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));

      expect(await screen.findByText('Initial value')).toBeDefined();
      await waitFor(() => expect(profile.mutationPending).toBe(false));
      expect(screen.queryByText('No current value.')).toBeNull();
      expect(screen.getByRole('button', { name: `Edit First ${cardinality} value 1` })).toBeDefined();
      await profile.setAttribute(created.slug, {
        value: { type: 'text', text: 'Replacement value' }, expected_value_id: 100,
        ...(cardinality === 'multi' ? { ordinal: 0 } : {})
      });

      expect(await screen.findByText('Replacement value')).toBeDefined();
      const history = screen.getByText('History (1)');
      await fireEvent.click(history);
      expect(screen.getByText('Initial value')).toBeDefined();
      expect(requests).toHaveLength(4);
      const writes = requests.filter((request) => request.method === 'PUT');
      expect(writes).toHaveLength(2);
      await expect(writes[0]!.clone().json()).resolves.toEqual({
        value: { type: 'text', text: 'Initial value' }, source: 'user'
      });
      await expect(writes[1]!.clone().json()).resolves.toEqual({
        value: { type: 'text', text: 'Replacement value' }, source: 'user', expected_value_id: 100,
        ...(cardinality === 'multi' ? { ordinal: 0 } : {})
      });
    }
  );

  it.each([
    ['Integer', '9007199254740992', 'JavaScript-safe whole number'],
    ['Integer', '-0', 'negative zero'],
    ['Integer', '-00', 'negative zero'],
    ['Number', '-0', 'negative zero'],
    ['Number', '1e20', 'ordinary decimal'],
    ['Number', '0.00001', 'ordinary decimal'],
    ['Number', '0x1p2', 'ordinary decimal'],
    ['Timestamp', '2026-01-01T00:00:00.1234567890Z', 'at most nine fractional digits'],
    ['Timestamp', '2026-01-01T00:00:00.120Z', 'omit trailing zero'],
    ['Timestamp', '2026-01-01T00:00:00+01:00', 'canonical UTC'],
    ['Timestamp', '0000-01-01T00:00:00+01:00', 'canonical UTC'],
    ['Timestamp', '9999-12-31T23:59:59-01:00', 'canonical UTC']
  ])('rejects a %s choice that cannot round-trip exactly through generated JSON: %s', async (type, choice, message) => {
    const fetchFn = vi.fn<typeof fetch>();
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Boundary choice' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), type);
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: choice } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect(screen.getByRole('alert').textContent).toContain(message);
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('clears stale type-specific options and omits a unit for numeric choices', async () => {
    const requests: Request[] = [];
    const created = definition({
      label: 'Boundary count', slug: 'boundary_count', value_type: 'integer', field_type: 'select',
      is_sensitive: false, options: { choices: [{ value: '1', label: 'One' }] }
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST' ? Response.json(created, { status: 201 }) : Response.json({ definitions: [created] });
    });
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Boundary count' } });
    await fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '5' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), 'Integer');
    expect(screen.queryByLabelText('Maximum length')).toBeNull();
    await fireEvent.input(screen.getByLabelText('Unit'), { target: { value: 'items' } });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: '1 | One' } });
    expect(screen.queryByLabelText('Unit')).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));

    await expect(requests[0]!.clone().json()).resolves.toMatchObject({
      value_type: 'integer', field_type: 'select', options: { choices: [{ value: '1', label: 'One' }] }
    });
    const body = await requests[0]!.clone().json() as { options: Record<string, unknown> };
    expect(body.options).not.toHaveProperty('unit');
    expect(body.options).not.toHaveProperty('max_length');
  });

  it('rejects a created definition that is not returned user-owned', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return Response.json(definition({ ownership: 'system' }), { status: 201 });
    });
    const profile = controller(fetchFn);
    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Wrong owner' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect((await screen.findByRole('alert')).textContent).toContain('user-owned');
    expect(requests).toHaveLength(1);
    expect(profile.createdDefinition).toBeNull();
  });

  it.each([
    ['the returned definition belongs to an organization', definition({ object_type: 'organization' }), []],
    ['the refreshed person registry omits the returned identity', definition(), [definition({ universal_id: 'different-id', slug: 'different_field' })]]
  ])('retains the draft and reports failure when %s', async (_case, returned, refreshed) => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.method === 'POST'
        ? Response.json(returned, { status: 201 })
        : Response.json({ definitions: refreshed });
    });
    const profile = controller(fetchFn);
    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Returned mismatch' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect((await screen.findByRole('alert')).textContent).toContain('person attribute registry');
    expect(profile.draft?.kind).toBe('createDefinition');
    expect(profile.createdDefinition).toBeNull();
  });

  it('retries only registry refresh after a committed POST until the exact refreshed identity appears', async () => {
    const created = definition({ is_sensitive: false });
    const requests: Request[] = [];
    let getAttempt = 0;
    let resolveSecondGet: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      getAttempt += 1;
      if (getAttempt === 1) return Response.json({ error: 'upstream', message: 'Registry temporarily unavailable' }, { status: 503 });
      if (getAttempt === 2) return new Promise<Response>((resolve) => { resolveSecondGet = resolve; });
      return Response.json({ definitions: [created] });
    });
    const onClose = vi.fn();
    const profile = controller(fetchFn);
    render(AttributeDefinitionDialog, { controller: profile, onClose });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Preferred channel' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Registry temporarily unavailable');
    expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
    expect(profile.definitionCreationCommit).toEqual({
      kind: 'target', universalID: created.universal_id, slug: created.slug
    });

    const retry = screen.getByRole('button', { name: 'Retry registry refresh' });
    await fireEvent.click(retry);
    await fireEvent.click(retry);
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(2);
    expect(retry).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Close create attribute field' }));
    expect(onClose).not.toHaveBeenCalled();

    resolveSecondGet?.(Response.json({ definitions: [] }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Retry registry refresh' })).toHaveProperty('disabled', false));
    await fireEvent.click(screen.getByRole('button', { name: 'Retry registry refresh' }));
    const result = await screen.findByRole('status');

    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(3);
    expect(profile.createdDefinition).toBe(profile.definitions[0]);
    expect(profile.definitionCreationCommit).toBeNull();
    expect(profile.draft).toBeNull();
    expect(document.activeElement).toBe(result);
  });

  it('keeps committed reconciliation controller-owned across dialog close and reopen', async () => {
    const created = definition({ is_sensitive: false });
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST'
        ? Response.json(created, { status: 201 })
        : Response.json({ definitions: [] });
    });
    const profile = controller(fetchFn);
    const onClose = vi.fn();
    const view = render(AttributeDefinitionDialog, { controller: profile, onClose });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Preferred channel' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await screen.findByRole('button', { name: 'Retry registry refresh' });
    await fireEvent.click(screen.getByRole('button', { name: 'Close create attribute field' }));
    expect(onClose).toHaveBeenCalledOnce();
    view.unmount();

    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    expect(screen.getByRole('button', { name: 'Retry registry refresh' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
    await expect(profile.createDefinition({
      object_type: 'person', label: 'Preferred channel', value_type: 'text', field_type: 'text',
      cardinality: 'single', is_sensitive: false
    })).resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('treats a successful POST without a usable identity as non-repeatable and offers reload only', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') {
        return Response.json({ ownership: 'user', object_type: 'person' }, { status: 201 });
      }
      if (new URL(request.url).pathname === '/api/v1/attribute-definitions') return Response.json({ definitions: [] });
      return Response.json({});
    });
    const profile = controller(fetchFn);
    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Unidentified field' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect((await screen.findByRole('alert')).textContent).toContain('usable identity');
    expect(profile.definitionCreationCommit).toEqual({ kind: 'unknown' });
    expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry registry refresh' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Reload Directory' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'GET')).toHaveLength(4));
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(profile.definitionCreationCommit).toEqual({ kind: 'unknown' });
  });

  it('treats an empty successful POST as non-repeatable', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST'
        ? new Response(null, { status: 204 })
        : Response.json({ definitions: [] });
    });
    const profile = controller(fetchFn);
    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Unidentified field' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect((await screen.findByRole('alert')).textContent).toContain('usable identity');
    expect(profile.definitionCreationCommit).toEqual({ kind: 'unknown' });
    expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
    await expect(profile.createDefinition({
      object_type: 'person', label: 'Unidentified field', value_type: 'text', field_type: 'text',
      cardinality: 'single', is_sensitive: false
    })).resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('treats a malformed successful POST as non-repeatable through the production API client', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return request.method === 'POST'
        ? new Response('{"ownership":"user",', { status: 201, headers: { 'Content-Type': 'application/json' } })
        : Response.json({ definitions: [] });
    });
    const profile = controller(fetchFn);
    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Malformed response field' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    expect((await screen.findByRole('alert')).textContent).toContain('usable identity');
    expect(profile.definitionCreationCommit).toEqual({ kind: 'unknown' });
    expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
    await expect(profile.createDefinition({
      object_type: 'person', label: 'Malformed response field', value_type: 'text', field_type: 'text',
      cardinality: 'single', is_sensitive: false
    })).resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
  });

  it('records a 201 before its body stream fails without observing another controller request', async () => {
    const decoy = definition({ id: 99, universal_id: 'decoy-id', slug: 'stream_field' });
    const requests: Request[] = [];
    let resolvePost: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return new Promise<Response>((resolve) => { resolvePost = resolve; });
      return Response.json({ definitions: [decoy] });
    });
    const client = createAPIClient(fetchFn);
    const initialBundle = { definitions: { definitions: [] }, etags: {}, errors: {} } satisfies DirectoryReadBundle;
    const profile = new DirectoryProfileController(client, 7, initialBundle);
    const concurrentProfile = new DirectoryProfileController(client, 8, initialBundle);
    render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Stream field' } });
    void fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(resolvePost).toBeDefined());

    await concurrentProfile.reloadDefinitions();
    expect(profile.definitionCreationCommit).toBeNull();

    const interruptedBody = new ReadableStream<Uint8Array>({
      start(stream) {
        stream.enqueue(new TextEncoder().encode('{"ownership":"user"'));
        stream.error(new Error('body interrupted after 201 headers'));
      }
    });
    resolvePost?.(new Response(interruptedBody, { status: 201, headers: { 'Content-Type': 'application/json' } }));

    expect((await screen.findByRole('alert')).textContent).toContain('body interrupted after 201 headers');
    expect(profile.definitionCreationCommit).toEqual({ kind: 'unknown' });
    expect(profile.createdDefinition).toBeNull();
    expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry registry refresh' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Reload Directory' })).toBeDefined();

    const retryBody = {
      object_type: 'person', label: 'Stream field', value_type: 'text', field_type: 'text',
      cardinality: 'single', is_sensitive: false
    } as const;
    await expect(profile.createDefinition(retryBody)).resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    await expect(profile.createDefinition(retryBody)).resolves.toEqual({ ok: false, code: 'conflict_unresolved' });
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);

    await fireEvent.click(screen.getByRole('button', { name: 'Reload Directory' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'GET')).toHaveLength(5));
    expect(profile.definitionCreationCommit).toEqual({ kind: 'unknown' });
    expect(profile.createdDefinition).toBeNull();
  });

  it('keeps one request pending, blocks every dismissal path, and renders the server error inline', async () => {
    let respond: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(() => new Promise<Response>((resolve) => { respond = resolve; }));
    const onClose = vi.fn();
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Pending field' } });
    const form = screen.getByRole('button', { name: 'Create field' }).closest('form')!;

    await fireEvent.submit(form);
    await fireEvent.submit(form);
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: 'Create field' })).toHaveProperty('disabled', true);
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    await fireEvent.keyDown(window, { key: 'Escape' });
    await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
    await fireEvent.click(screen.getByRole('button', { name: 'Close create attribute field' }));
    expect(onClose).not.toHaveBeenCalled();

    respond?.(Response.json({ error: 'attribute_invalid', message: 'Synthetic server validation failure' }, { status: 400 }));
    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic server validation failure');
    expect(screen.getByRole('button', { name: 'Create field' })).toHaveProperty('disabled', false);
  });

  it.each([
    {
      name: 'integer', option: 'Integer', slug: 'safe_integer_choice',
      choices: ['-9007199254740991', '9007199254740991'],
      values: [{ type: 'integer', integer: -9007199254740991 }, { type: 'integer', integer: 9007199254740991 }]
    },
    {
      name: 'real', option: 'Number', slug: 'safe_real_choice',
      choices: ['-999999.5', '0', '0.0001', '999999.5'],
      values: [
        { type: 'real', real: -999999.5 }, { type: 'real', real: 0 },
        { type: 'real', real: 0.0001 }, { type: 'real', real: 999999.5 }
      ]
    },
    {
      name: 'timestamp', option: 'Timestamp', slug: 'safe_timestamp_choice',
      choices: ['0000-01-01T00:00:00Z', '2026-01-01T00:00:00.123456789Z', '9999-12-31T23:59:59Z'],
      values: [
        { type: 'timestamp', timestamp: '0000-01-01T00:00:00Z' },
        { type: 'timestamp', timestamp: '2026-01-01T00:00:00.123456789Z' },
        { type: 'timestamp', timestamp: '9999-12-31T23:59:59Z' }
      ]
    }
  ])('creates, refetches, selects, and saves every accepted $name boundary choice through AttributeEditor', async ({ option, slug, choices, values }) => {
    const created = definition({
      id: 73, universal_id: `created-${slug}`, slug, label: 'Boundary choice',
      value_type: values[0]!.type, field_type: 'multiselect', cardinality: 'multi', is_sensitive: false,
      options: { choices: choices.map((value) => ({ value, label: value })) }
    });
    const requests: Request[] = [];
    let valueID = 100;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      if (request.method === 'GET') return Response.json({ definitions: [created] });
      const body = await request.clone().json() as components['schemas']['SetPersonAttributeRequest'];
      return Response.json({
        dry_run: false,
        value: {
          id: valueID++, person_id: 7, definition_id: created.id, definition_slug: slug,
          ordinal: body.ordinal ?? 0, value: body.value, active_from: when, created_at: when,
          source: 'user', actor: 'synthetic-user'
        }
      });
    });
    const profile = controller(fetchFn);
    render(AttributeSection, { controller: profile });
    await fireEvent.click(screen.getByRole('button', { name: 'Create attribute field' }));
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Boundary choice' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), option);
    await chooseSelectOption(screen.getByLabelText('Cardinality'), 'Multiple values');
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: choices.join('\n') } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await screen.findByRole('status');
    await fireEvent.click(screen.getByRole('button', { name: 'Done' }));

    for (const [index, choice] of choices.entries()) {
      await fireEvent.click(screen.getByRole('button', { name: 'Add Boundary choice value' }));
      await fireEvent.change(screen.getByRole('combobox', { name: 'Boundary choice' }), { target: { value: choice } });
      await fireEvent.click(screen.getByRole('button', { name: 'Save attribute' }));
      await waitFor(() => expect(requests.filter((request) => request.method === 'PUT')).toHaveLength(index + 1));
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Save attribute' })).toBeNull());
    }

    const definitionBody = await requests.find((request) => request.method === 'POST')!.clone().json() as Record<string, unknown>;
    expect(definitionBody).toMatchObject({ options: { choices: created.options!.choices } });
    const writes = await Promise.all(requests.filter((request) => request.method === 'PUT').map((request) => request.clone().json()));
    expect(writes).toEqual(values.map((value) => ({ value, source: 'user' })));
  });

  it.each([
    { option: 'Text', slug: 'short_note', options: { max_length: 5 }, fill: async () => fireEvent.input(screen.getByLabelText('Maximum length'), { target: { value: '5' } }), visible: '0 / 5 characters.' },
    { option: 'Integer', slug: 'duration_days', options: { unit: 'days' }, fill: async () => fireEvent.input(screen.getByLabelText('Unit'), { target: { value: 'days' } }), visible: 'days' }
  ])('makes a created $option option visible in the production editor', async ({ option, slug, options, fill, visible }) => {
    const created = definition({
      universal_id: `created-${slug}`, slug, label: 'Option field', value_type: option === 'Text' ? 'text' : 'integer',
      field_type: option === 'Text' ? 'text' : 'duration', is_sensitive: false, options
    });
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.method === 'POST' ? Response.json(created, { status: 201 }) : Response.json({ definitions: [created] });
    });
    render(AttributeSection, { controller: controller(fetchFn) });
    await fireEvent.click(screen.getByRole('button', { name: 'Create attribute field' }));
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Option field' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), option);
    await fill();
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await screen.findByRole('status');
    await fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Add Option field value' }));

    expect(screen.getByRole('form', { name: 'Add Option field value' }).textContent).toContain(visible);
  });

  it('refreshes definitions without replacing the selected attribute draft and makes the new field immediately usable', async () => {
    const existing = definition({
      id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'existing_field',
      label: 'Existing field', field_type: 'text', is_sensitive: false, options: undefined
    });
    const created = definition({ is_sensitive: false });
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      return Response.json({ definitions: [existing, created] });
    });
    const profile = controller(fetchFn, [existing]);
    render(AttributeSection, { controller: profile });
    await fireEvent.click(screen.getByRole('button', { name: 'Add Existing field value' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Existing field' }), { target: { value: 'retained local draft' } });
    const createTrigger = screen.getByRole('button', { name: 'Create attribute field' });
    createTrigger.focus();
    await fireEvent.click(createTrigger);
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Preferred channel' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await screen.findByRole('status');
    await fireEvent.click(screen.getByRole('button', { name: 'Done' }));

    await waitFor(() => expect(document.activeElement).toBe(createTrigger));
    expect(profile.createdDefinition).toBeNull();
    expect(screen.getByRole('textbox', { name: 'Existing field' })).toHaveProperty('value', 'retained local draft');
    expect(screen.getByRole('button', { name: 'Add Preferred channel value' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Add Preferred channel value' }));
    expect(screen.getByRole('combobox', { name: 'Preferred channel' })).toBeDefined();
  });
});
