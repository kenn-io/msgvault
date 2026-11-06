import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { employment, organization, requestOf } from '../../directory/entity-fixtures.test-support';
import { DirectoryController } from '../../directory/controller.svelte';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { personDirectoryContext } from '../../directory/person-directory-context';
import PersonFacts from './PersonFacts.svelte';

afterEach(cleanup);

it('loads Claims only after Show and opens setup from its menu', async () => {
  const fetchFn = vi.fn<typeof fetch>(async input => {
    const path = new URL(requestOf(input).url).pathname;
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: [] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
    if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
    return Response.json({ decisions: [] });
  });
  const onSetUp = vi.fn();
  const client = createAPIClient(fetchFn);
  const directory = new DirectoryController(client);
  render(PersonFacts, { props: { client, personID: 42, onSetUp }, context: new Map([[personDirectoryContext, () => directory]]) });
  expect(fetchFn).toHaveBeenCalledTimes(0);
  await fireEvent.click(screen.getByRole('button', { name: 'Facts actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Set up Facts…' }));
  await vi.waitFor(() => expect(onSetUp).toHaveBeenCalledOnce());
  await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
  await screen.findByText('No claims yet');
  expect(screen.getByRole('radio', { name: 'Claims' }).getAttribute('aria-checked')).toBe('true');
  directory.destroy();
});

it('refreshes saved pins when employment is saved while Facts is open', async () => {
  let saved = false;
  const client = createAPIClient(async input => {
    const request = requestOf(input), path = new URL(request.url).pathname;
    if (path.endsWith('/employments') && request.method === 'POST') {
      saved = true;
      return Response.json(employment(11, 1, { person_id: 42 }), { headers: { ETag: '"1"' } });
    }
    if (path.endsWith('/employments')) return Response.json({ employments: saved ? [employment(11, 1, { person_id: 42 })] : [] });
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: saved ? [{ target: { kind: 'employment', key: 'system:employment', revision: `sha256:${'a'.repeat(64)}` }, pinned: true }] : [] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
    if (path.endsWith('/network')) return Response.json({ nodes: [], edges: [] });
    throw new Error(`Unexpected ${request.method} ${path}`);
  });
  const directory = new DirectoryController(client);
  directory.selectedPersonID = 42;
  const entity = new DirectoryEntityController(client, 42);
  entity.organizations = [organization()];
  directory.entity = entity;
  render(PersonFacts, { props: { client, personID: 42 }, context: new Map([[personDirectoryContext, () => directory]]) });
  await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
  await screen.findByText('No claims yet');
  expect(screen.queryByText('Saved pinned values')).toBeNull();
  expect(await entity.createEmployment({ person_id: 42, organization_id: 21, source: 'user' })).toMatchObject({ ok: true });
  await screen.findByText('Engineer · Synthetic Org');
  directory.destroy();
});

it.each([
  { operation: 'save', section: 'Claims' },
  { operation: 'clear', section: 'Claims' },
  { operation: 'save', section: 'Pins' },
  { operation: 'clear', section: 'Pins' },
] as const)('refreshes saved values after an attribute $operation while $section stays open', async ({ operation, section }) => {
  const when = '2026-08-01T00:00:00Z';
  const definition = { id: 8, universal_id: '00000000-0000-4000-8000-000000000008', slug: 'location', label: 'Location', value_type: 'text', field_type: 'text', cardinality: 'single', object_type: 'person', ownership: 'user', display_order: 1, is_active: true, api_mutable: true, ui_editable: true, ui_creatable: true, is_sensitive: false, revision: 1 };
  const value = { id: 41, person_id: 42, definition_id: 8, definition_slug: 'location', ordinal: 0, value: { type: 'text', text: 'Example City' }, active_from: when, created_at: when, source: 'user' };
  let saved = false;
  const client = createAPIClient(async input => {
    const request = requestOf(input), path = new URL(request.url).pathname;
    if (path.endsWith('/attributes/location')) {
      saved = true;
      return Response.json(operation === 'save' ? { value } : { superseded: value });
    }
    if (path.endsWith('/people/42')) return Response.json({ id: 42, revision: saved ? 2 : 1 });
    if (path.endsWith('/attributes')) return Response.json({ person_id: 42, attributes: [{ definition, current: (operation === 'save' ? saved : !saved) ? [value] : [], history: [] }] });
    if (path.endsWith('/attribute-definitions')) return Response.json({ definitions: [definition] });
    if (path.endsWith('/fact-pins')) return Response.json({ pins: saved ? [{ target: { kind: 'attribute', key: definition.universal_id, revision: `sha256:${'a'.repeat(64)}` }, pinned: true }] : [] });
    if (path.endsWith('/person-fact-targets')) return Response.json({ targets: [] });
    if (path.endsWith('/fact-claims')) return Response.json({ claims: [] });
    if (path.endsWith('/fact-evidence')) return Response.json({ evidence: [] });
    if (path.endsWith('/fact-decisions')) return Response.json({ decisions: [] });
    return Response.json({});
  });
  const directory = new DirectoryController(client);
  await directory.loadSelection(42);
  const profile = directory.profile!;
  const view = render(PersonFacts, { props: { client, personID: 42, refreshKey: profile.person }, context: new Map([[personDirectoryContext, () => directory]]) });
  await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
  await screen.findByText('No claims yet');
  if (section === 'Pins') {
    await fireEvent.click(screen.getByRole('radio', { name: 'Pins' }));
    await screen.findByText('No pinned fields yet');
  }
  if (operation === 'save') await profile.setAttribute('location', { value: { type: 'text', text: 'Example City' } });
  else await profile.clearAttribute('location', 41);
  await view.rerender({ client, personID: 42, refreshKey: profile.person });
  await screen.findByText(operation === 'save' ? 'Example City' : 'No saved value');
  expect(screen.getByRole('radio', { name: section }).getAttribute('aria-checked')).toBe('true');
  const savedValues = screen.getByRole('region', { name: 'Saved pinned values' });
  if (section === 'Claims') expect(savedValues.compareDocumentPosition(screen.getByRole('region', { name: 'Claims' })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  directory.destroy();
});
