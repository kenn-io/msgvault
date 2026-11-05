import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { requestOf } from '../../directory/entity-fixtures.test-support';
import { DirectoryController } from '../../directory/controller.svelte';
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
