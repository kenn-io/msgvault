import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { employment, organization, requestOf } from '../../directory/entity-fixtures.test-support';
import { DirectoryController } from '../../directory/controller.svelte';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import { personDirectoryContext } from '../../directory/person-directory-context';
import PersonFacts from './PersonFacts.svelte';

afterEach(cleanup);

it('loads Claims only after Show, clears on Hide, and opens setup from its menu', async () => {
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
  const view = render(PersonFacts, { props: { client, personID: 42, onSetUp }, context: new Map([[personDirectoryContext, () => directory]]) });
  expect(fetchFn).toHaveBeenCalledTimes(0);
  await fireEvent.click(screen.getByRole('button', { name: 'Facts actions' }));
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Set up' }));
  await vi.waitFor(() => expect(onSetUp).toHaveBeenCalledOnce());
  await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
  await screen.findByText('No claims yet');
  expect(screen.getByRole('radio', { name: 'Claims' }).getAttribute('aria-checked')).toBe('true');
  expect(screen.queryByRole('navigation', { name: 'claims pages' })).toBeNull();
  fetchFn.mockClear();
  await view.rerender({ client, personID: 42, onSetUp, refreshKey: 1 });
  await vi.waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
  expect(fetchFn.mock.calls.map(([input]) => new URL(requestOf(input).url).pathname).sort()).toEqual(['/api/v1/people/42/fact-claims', '/api/v1/people/42/fact-pins']);
  await fireEvent.click(screen.getByRole('button', { name: 'Hide' }));
  expect(within(screen.getByRole('region', { name: 'Facts' })).queryByText('No claims yet')).toBeNull();
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
