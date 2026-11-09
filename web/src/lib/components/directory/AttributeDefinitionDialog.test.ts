import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type {
  AttributeDefinition as GeneratedAttributeDefinition,
} from '../../api/generated/models';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';
import AttributeDefinitionDialog from './AttributeDefinitionDialog.svelte';
import PeopleCatalogSettings from '../settings/PeopleCatalogSettings.svelte';

type AttributeDefinition = GeneratedAttributeDefinition;

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
    options: {
      choices: [
        { value: 'email', label: 'email' },
        { value: 'phone', label: 'phone' },
      ],
    },
    is_active: true,
    revision: 1,
    created_at: when,
    updated_at: when,
    ...overrides,
  };
}

function controller(fetchFn: typeof fetch, definitions: AttributeDefinition[] = []): DirectoryProfileController {
  const bundle = {
    definitions: { definitions },
    etags: {},
    errors: {},
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
    await fireEvent.input(screen.getByLabelText('Description'), {
      target: { value: ' How this person prefers to be contacted ' },
    });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: ' email\nphone ' } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Sensitive' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));

    await waitFor(() => expect(profile.createdDefinition).toEqual(created));
    expect(requests).toHaveLength(2);
    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', '/api/v1/attribute-definitions'],
      ['GET', '/api/v1/attribute-definitions'],
    ]);
    const body = (await requests[0]!.clone().json()) as Record<string, unknown>;
    expect(body).toEqual({
      object_type: 'person',
      label: 'Preferred channel',
      description: 'How this person prefers to be contacted',
      value_type: 'text',
      field_type: 'select',
      cardinality: 'single',
      is_sensitive: true,
      options: {
        choices: [
          { value: 'email', label: 'email' },
          { value: 'phone', label: 'phone' },
        ],
      },
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

  it.each<{ optionLabel: string; valueType: string; fieldType: string; recordTarget?: string; cardinality?: string; choices?: string; expected?: Array<{ value: string; label: string }> }>([
    { optionLabel: 'Text', valueType: 'text', fieldType: 'text' },
    { optionLabel: 'Integer', valueType: 'integer', fieldType: 'duration' },
    { optionLabel: 'Number', valueType: 'real', fieldType: 'text' },
    { optionLabel: 'Boolean', valueType: 'boolean', fieldType: 'checkbox' },
    { optionLabel: 'Date', valueType: 'date', fieldType: 'date' },
    { optionLabel: 'Timestamp', valueType: 'timestamp', fieldType: 'timestamp' },
    { optionLabel: 'JSON', valueType: 'json', fieldType: 'json' },
    { optionLabel: 'Person reference', valueType: 'record_reference', fieldType: 'person', recordTarget: 'person' },
    { optionLabel: 'Text', valueType: 'text', fieldType: 'multiselect', cardinality: 'multi', choices: ' email | Email\n phone | Phone ', expected: [{ value: 'email', label: 'Email' }, { value: 'phone', label: 'Phone' }] },
    { optionLabel: 'Integer', valueType: 'integer', fieldType: 'select', choices: '1 | One', expected: [{ value: '1', label: 'One' }] },
  ])('sends a server-compatible $optionLabel definition ($fieldType)', async ({ optionLabel, valueType, fieldType, recordTarget, cardinality = 'single', choices, expected }) => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const created = definition({
        label: 'Synthetic field',
        slug: 'synthetic_field',
        value_type: valueType,
        field_type: fieldType,
        ...(recordTarget ? { record_target: recordTarget } : {}),
        is_sensitive: false,
        cardinality,
        options: expected ? { choices: expected } : undefined,
      });
      if (request.method === 'POST') return Response.json(created, { status: 201 });
      return Response.json({ definitions: [created] });
    });

    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Synthetic field' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), optionLabel);
    if (cardinality === 'multi') await chooseSelectOption(screen.getByLabelText('Cardinality'), 'Multiple values');
    if (choices) await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: choices } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));

    const body = (await requests[0]!.clone().json()) as Record<string, unknown>;
    expect(body).toMatchObject({ value_type: valueType, field_type: fieldType, cardinality });
    if (expected) expect(body.options).toEqual({ choices: expected });
    if (recordTarget) expect(body).toHaveProperty('record_target', recordTarget);
    else expect(body).not.toHaveProperty('record_target');
  });

  it.each([
    ['a blank label', '', '', 'Enter a label.'],
    ['a choice missing its value', 'Field', ' | Visible label', 'Each choice needs a value.'],
    ['a choice missing its label', 'Field', 'value | ', 'Each choice needs a label.'],
    ['trim-equivalent choices', 'Field', ' email | Email\nemail | Other', 'Choice values must be unique.'],
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
      return request.method === 'POST'
        ? Response.json(created, { status: 201 })
        : Response.json({ definitions: [created] });
    });
    render(AttributeDefinitionDialog, { controller: controller(fetchFn), onClose: vi.fn() });
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'JSON field' } });
    await fireEvent.input(screen.getByLabelText('Choices'), { target: { value: 'stale choice' } });
    await chooseSelectOption(screen.getByLabelText('Value type'), 'JSON');
    expect(screen.queryByLabelText('Choices')).toBeNull();

    await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    const body = (await requests[0]!.clone().json()) as Record<string, unknown>;
    expect(body).not.toHaveProperty('options');
  });

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
    ['Timestamp', '9999-12-31T23:59:59-01:00', 'canonical UTC'],
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
    [
      'the refreshed person registry omits the returned identity',
      definition(),
      [definition({ universal_id: 'different-id', slug: 'different_field' })],
    ],
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
      if (getAttempt === 1)
        return Response.json({ error: 'upstream', message: 'Registry temporarily unavailable' }, { status: 503 });
      if (getAttempt === 2)
        return new Promise<Response>((resolve) => {
          resolveSecondGet = resolve;
        });
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
      kind: 'target',
      universalID: created.universal_id,
      slug: created.slug,
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
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Retry registry refresh' })).toHaveProperty('disabled', false),
    );
    await fireEvent.click(screen.getByRole('button', { name: 'Retry registry refresh' }));
    const result = await screen.findByRole('status');

    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(1);
    expect(requests.filter((request) => request.method === 'GET')).toHaveLength(3);
    expect(profile.createdDefinition).toBe(profile.definitions[0]);
    expect(profile.definitionCreationCommit).toBeNull();
    expect(profile.draft).toBeNull();
    expect(document.activeElement).toBe(result);
  });

  it('keeps a committed field creation resumable after closing its catalog dialog', async () => {
    const created = definition({ is_sensitive: false });
    const requests: Request[] = [];
    let committed = false;
    const client = createAPIClient(async input => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      requests.push(request);
      if (path.endsWith('/attribute-definitions')) {
        if (request.method === 'POST') { committed = true; return Response.json(created, { status: 201 }); }
        return committed ? Response.json({ message: 'Registry temporarily unavailable' }, { status: 503 }) : Response.json({ definitions: [] });
      }
      if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
      if (path.endsWith('/relationship-types')) return Response.json({ relationship_types: [] });
      if (path.endsWith('/organizations')) return Response.json({ organizations: [], total: 0, limit: 50, offset: 0 });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const view = render(PeopleCatalogSettings, { client });
    try {
      await fireEvent.click(await screen.findByRole('button', { name: 'Add field' }));
      await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Preferred channel' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
      await screen.findByRole('button', { name: 'Retry registry refresh' });
      await fireEvent.click(screen.getByRole('button', { name: 'Close create attribute field' }));
      expect(screen.queryByRole('dialog', { name: 'Create attribute field' })).toBeNull();
      expect(screen.getByRole('button', { name: 'Add field' })).toHaveProperty('disabled', true);
      await fireEvent.click(screen.getByRole('button', { name: 'Resume field creation' }));
      const dialog = await screen.findByRole('dialog', { name: 'Create attribute field' });
      await fireEvent.click(within(dialog).getByRole('button', { name: 'Retry registry refresh' }));
      await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Retry registry refresh' })).toHaveProperty('disabled', false));
      expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    } finally { view.unmount(); }
  });

  it.each([
    ['without a usable identity', () => Response.json({ ownership: 'user', object_type: 'person' }, { status: 201 }), 'usable identity'],
    ['with an empty body', () => new Response(null, { status: 204 }), 'usable identity'],
    ['with malformed JSON', () => new Response('{"ownership":"user",', { status: 201, headers: { 'Content-Type': 'application/json' } }), 'usable identity'],
    ['with an interrupted body', () => new Response(new ReadableStream<Uint8Array>({
      start(stream) { stream.error(new Error('body interrupted after 201 headers')); },
    }), { status: 201, headers: { 'Content-Type': 'application/json' } }), 'body interrupted after 201 headers'],
  ] as const)('treats a successful POST %s as non-repeatable and offers reload only', async (_shape, response, error) => {
    let resolvePost!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') return new Promise<Response>(resolve => { resolvePost = resolve; });
      return Response.json({ definitions: [definition({ id: 99, universal_id: 'decoy-id', slug: 'stream_field' })] });
    });
    const client = createAPIClient(fetchFn);
    const profile = new DirectoryProfileController(client, 7, { definitions: { definitions: [] }, etags: {}, errors: {} });
    const view = render(AttributeDefinitionDialog, { controller: profile, onClose: vi.fn() });
    try {
      await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'Unidentified field' } });
      await fireEvent.click(screen.getByRole('button', { name: 'Create field' }));
      await waitFor(() => expect(resolvePost).toBeDefined());
      resolvePost(response());
      expect((await screen.findByRole('alert')).textContent).toContain(error);
      expect(screen.queryByRole('button', { name: 'Create field' })).toBeNull();
      expect(screen.queryByRole('button', { name: 'Retry registry refresh' })).toBeNull();
      await fireEvent.click(screen.getByRole('button', { name: 'Reload fields' }));
      await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('Reload Directory before creating another field.'));
      await waitFor(() => expect(screen.getByRole('button', { name: 'Reload fields' })).toHaveProperty('disabled', false));
    } finally { view.unmount(); profile.destroy(); }
  });

  it('keeps one request pending, blocks every dismissal path, and renders the server error inline', async () => {
    let respond: ((response: Response) => void) | undefined;
    const fetchFn = vi.fn<typeof fetch>(
      () =>
        new Promise<Response>((resolve) => {
          respond = resolve;
        }),
    );
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

    respond?.(
      Response.json({ error: 'attribute_invalid', message: 'Synthetic server validation failure' }, { status: 400 }),
    );
    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic server validation failure');
    expect(screen.getByRole('button', { name: 'Create field' })).toHaveProperty('disabled', false);
  });

});
