import { pathOf } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryController } from '../../directory/controller.svelte';
import type { DirectoryURLState } from '../../directory/models';
import { chooseSelectOption } from '../../../test/kit-ui';
import DirectoryWorkspace from './DirectoryWorkspace.svelte';

const state: DirectoryURLState = {
  directoryQuery: '', directoryContactState: '', directoryCategory: '',
  directoryOrganization: '', directoryPrimaryChannel: '', directoryLastContactAfter: '', directoryLastContactBefore: '', directorySort: 'name', directoryPersonID: null
};

function directoryResponse() {
  return Response.json({
    people: [{
      id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active',
      categories: ['friend'], organizations: ['Example Org'], primary_channel: 'email'
    }]
  });
}

describe('DirectoryWorkspace', () => {
  afterEach(() => { vi.useRealTimers(); });

  it('sends one request and replaces one history entry after a burst of typing', async () => {
    vi.useFakeTimers();
    const commits: Array<[Partial<DirectoryURLState>, string]> = [];
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client, (patch, history) => commits.push([patch, history]));
    render(DirectoryWorkspace, { client, controller, state });
    await vi.advanceTimersByTimeAsync(0);
    expect(requests).toHaveLength(1);

    const search = screen.getByRole('searchbox', { name: 'Search directory' }) as HTMLInputElement;
    for (const typed of ['a', 'al', 'ali', 'alic', 'alice']) {
      await fireEvent.input(search, { target: { value: typed } });
      await vi.advanceTimersByTimeAsync(100);
    }
    expect(search.value).toBe('alice');
    expect(requests).toHaveLength(1);
    expect(commits).toEqual([]);

    await vi.advanceTimersByTimeAsync(250);
    expect(commits).toEqual([[{
      directoryQuery: 'alice', directoryCategory: '', directoryOrganization: ''
    }, 'replace']]);
    expect(requests).toHaveLength(2);
    expect(new URL(requests[1]!.url).searchParams.get('q')).toBe('alice');
    expect(controller.query).toBe('alice');
  });

  it('applies a pending text edit before a select filter pushes', async () => {
    vi.useFakeTimers();
    const commits: Array<[Partial<DirectoryURLState>, string]> = [];
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client, (patch, history) => commits.push([patch, history]));
    render(DirectoryWorkspace, { client, controller, state });
    await vi.advanceTimersByTimeAsync(0);

    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Organization filter' }), { target: { value: 'Example Org' } });
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Contact state:/ }), 'Active');
    await vi.advanceTimersByTimeAsync(0);

    expect(commits.map(([, history]) => history)).toEqual(['replace', 'push']);
    const last = new URL(requests.at(-1)!.url).searchParams;
    expect(last.get('organization')).toBe('Example Org');
    expect(last.get('contact_state')).toBe('active');
  });

  it('keeps the current rows visible while a filter change loads', async () => {
    let resolveFiltered: ((response: Response) => void) | undefined;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).searchParams.get('contact_state') === 'active') {
        return new Promise<Response>((resolve) => { resolveFiltered = resolve; });
      }
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });
    await screen.findByRole('row', { name: /Synthetic Person/ });

    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Contact state:/ }), 'Active');
    await waitFor(() => expect(resolveFiltered).toBeDefined());
    expect(screen.getByRole('row', { name: /Synthetic Person/ })).toBeDefined();
    expect(screen.getByRole('status').textContent).toBe('Updating people…');
    expect(screen.queryByText('Loading people…')).toBeNull();

    resolveFiltered?.(Response.json({ people: [{ id: 8, revision: 1, display_name: 'Filtered Person', contact_state: 'active', categories: [], organizations: [] }] }));
    await screen.findByRole('row', { name: /Filtered Person/ });
    expect(screen.queryByRole('row', { name: /Synthetic Person/ })).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('offers free-text category and organization filters when no server facet catalog exists', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);

    render(DirectoryWorkspace, { client, controller, state });
    expect(screen.queryByRole('textbox', { name: 'Category filter' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));

    expect(screen.getByRole('textbox', { name: 'Category filter' })).toBeDefined();
    expect(screen.getByRole('textbox', { name: 'Organization filter' })).toBeDefined();
  });

  it('offers last-contact range and ordering controls', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);

    render(DirectoryWorkspace, { client, controller, state });
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));

    expect((screen.getByLabelText('Last contacted after') as HTMLInputElement).type).toBe('date');
    expect((screen.getByLabelText('Last contacted before') as HTMLInputElement).type).toBe('date');
    expect(screen.getByRole('combobox', { name: /^Directory order:/ })).toBeDefined();
  });

  it('sets and clears each last-contacted boundary on its own', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));

    await fireEvent.change(screen.getByLabelText('Last contacted after'), { target: { value: '2024-01-05' } });
    expect(controller.lastContactAfter).toBe('2024-01-05');
    expect(controller.lastContactBefore).toBe('');
    expect(screen.getByText('Last contacted after Jan 5, 2024')).toBeDefined();
    await waitFor(() => {
      const params = new URL(requests.at(-1)!.url).searchParams;
      expect(params.get('last_contact_after')).toBe('2024-01-05T00:00:00Z');
      expect(params.has('last_contact_before')).toBe(false);
    });

    await fireEvent.click(screen.getByRole('button', { name: 'Remove Last contacted after Jan 5, 2024 filter' }));
    expect(controller.lastContactAfter).toBe('');
    expect(screen.queryByText('Last contacted after Jan 5, 2024')).toBeNull();
  });

  it('clears a boundary from the clear button beside its date field', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state: { ...state, directoryLastContactBefore: '2024-03-01' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));

    expect(screen.getByText('Last contacted before Mar 1, 2024')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Clear last contacted before' }));
    expect(controller.lastContactBefore).toBe('');
    expect((screen.getByLabelText('Last contacted before') as HTMLInputElement).value).toBe('');
  });

  it('removes only the chip that was cleared', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, {
      client, controller,
      state: { ...state, directoryContactState: 'active', directoryOrganization: 'Example Co', directoryPrimaryChannel: 'email' }
    });

    expect(screen.getByText('Primary channel: Email')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Contact state: Active filter' }));
    expect(controller.contactState).toBe('');
    expect(controller.organization).toBe('Example Co');
    expect(controller.primaryChannel).toBe('email');
    expect(screen.getByText('Organization: Example Co')).toBeDefined();
    expect(screen.queryByText('Contact state: Active')).toBeNull();
  });

  it('names primary channels and sorts with a visible label', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => directoryResponse()));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    expect(screen.getByRole('combobox', { name: /^Directory order:/ }).textContent).toContain('Sort: Name');
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
    await fireEvent.click(screen.getByRole('combobox', { name: /^Primary channel/ }));
    expect(screen.getAllByRole('option').map((option) => option.textContent?.trim())).toEqual([
      'All channels', 'Email', 'Chat', 'Meeting', 'Other'
    ]);
  });

  it.each([
    ['email', 'Email'], ['chat', 'Chat'], ['meeting', 'Meeting'], ['other', 'Other']
  ])('filters by the %s activity channel and clears it', async (channel, label) => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });
    await screen.findByRole('row', { name: /Synthetic Person/ });
    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Primary channel/ }), label);
    await waitFor(() => expect(new URL(requests.at(-1)!.url).searchParams.get('primary_channel')).toBe(channel));
    expect(screen.getByText(`Primary channel: ${label}`)).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: `Remove Primary channel: ${label} filter` }));
    await waitFor(() => expect(new URL(requests.at(-1)!.url).searchParams.has('primary_channel')).toBe(false));
    expect(screen.queryByText(`Primary channel: ${label}`)).toBeNull();
  });

  it('counts people and marks more pages with a plus', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json({
      people: [{ id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active', categories: [], organizations: [] }],
      next_cursor: 'more'
    })));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    expect(await screen.findByText('1+ people')).toBeDefined();
  });

  it('offers only contact states accepted by the Directory handler contract', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return directoryResponse();
    }));
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
    const contactState = screen.getByRole('combobox', { name: /^Contact state:/ });
    await chooseSelectOption(contactState, 'Active');
    await waitFor(() => expect(new URL(requests.at(-1)!.url).searchParams.get('contact_state')).toBe('active'));
    await chooseSelectOption(contactState, 'Inactive');
    await waitFor(() => expect(new URL(requests.at(-1)!.url).searchParams.get('contact_state')).toBe('inactive'));
    await fireEvent.click(contactState);
    expect(screen.queryByRole('option', { name: /unknown/i })).toBeNull();
  });

  it('keeps loaded rows visible when loading another page fails and retries that page', async () => {
    let pageRequests = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/people/directory') throw new Error(`unexpected ${pathOf(request)}`);
      pageRequests += 1;
      if (pageRequests === 1) return Response.json({
        people: [{ id: 7, revision: 2, display_name: 'Synthetic Person', contact_state: 'active', categories: [], organizations: [] }],
        next_cursor: 'next'
      });
      if (pageRequests === 2) throw new Error('network offline');
      return Response.json({ people: [{ id: 8, revision: 1, display_name: 'Fixture Person', contact_state: 'unknown', categories: [], organizations: [] }] });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    expect(await screen.findByText('Synthetic Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Load more people' }));
    expect((await screen.findByRole('alert')).textContent).toContain('network offline');
    expect(screen.getByText('Synthetic Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry loading more people' }));
    expect(await screen.findByText('Fixture Person')).toBeDefined();
  });

  it('offers a page-one reload after a terminal cursor failure and retains rows until success', async () => {
    let pageRequests = 0;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (pathOf(request) !== '/api/v1/people/directory') throw new Error(`unexpected ${pathOf(request)}`);
      pageRequests += 1;
      if (pageRequests === 1) return Response.json({
        people: [{ id: 7, revision: 2, display_name: 'Retained Person', contact_state: 'active', categories: [], organizations: [] }],
        next_cursor: 'invalidated'
      });
      if (pageRequests === 2) return Response.json({ error: 'invalid_cursor', message: 'Directory changed' }, { status: 400 });
      return Response.json({
        people: [{ id: 8, revision: 1, display_name: 'Reloaded Person', contact_state: 'active', categories: [], organizations: [] }]
      });
    });
    const client = createAPIClient(fetchFn);
    const controller = new DirectoryController(client);
    render(DirectoryWorkspace, { client, controller, state });

    expect(await screen.findByText('Retained Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Load more people' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Directory changed');
    expect(screen.getByText('Retained Person')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Reload directory' }));
    expect(await screen.findByText('Reloaded Person')).toBeDefined();
  });

  describe('bulk deletion', () => {
    const person = (id: number, name: string) => ({
      id, revision: 1, display_name: name, contact_state: 'active', categories: [], organizations: []
    });

    /** Answers DELETE /api/v1/people/{id} from `deletes` and every list read
     * from `pages` in order, recording which list reads asked for page one. */
    function renderDirectory(
      pages: Array<{ people: ReturnType<typeof person>[]; next_cursor?: string } | (() => Response)>,
      deletes: Record<number, () => Response>
    ) {
      const pageOneReads: number[] = [];
      let listReads = 0;
      const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        const match = /^\/api\/v1\/people\/(\d+)$/.exec(pathOf(request));
        if (request.method === 'DELETE' && match) return deletes[Number(match[1])]!();
        listReads += 1;
        if (!new URL(request.url).searchParams.get('cursor')) pageOneReads.push(listReads);
        const page = pages.shift() ?? { people: [] };
        return typeof page === 'function' ? page() : Response.json(page);
      }));
      render(DirectoryWorkspace, { client, controller: new DirectoryController(client), state });
      return { pageOneReads };
    }

    async function deleteAllLoaded(): Promise<void> {
      await fireEvent.click(await screen.findByRole('checkbox', { name: 'Select all loaded people' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Delete…' }));
      await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }));
    }

    it('reloads page one after deleting every loaded person, so later pages stay reachable', async () => {
      const { pageOneReads } = renderDirectory(
        [
          { people: [person(1, 'Alpha Fixture'), person(2, 'Bravo Fixture')], next_cursor: 'cursor-after-bravo' },
          { people: [person(3, 'Charlie Fixture')] }
        ],
        { 1: () => new Response(null, { status: 204 }), 2: () => new Response(null, { status: 204 }) }
      );

      await deleteAllLoaded();

      expect(await screen.findByText('2 people deleted.')).toBeDefined();
      expect(screen.getByRole('row', { name: /Charlie Fixture/ })).toBeDefined();
      expect(pageOneReads).toEqual([1, 2]);
    });

    it('keeps the deletion outcome visible when the page-one reload fails', async () => {
      renderDirectory(
        [
          { people: [person(1, 'Alpha Fixture'), person(2, 'Bravo Fixture')] },
          () => Response.json({ error: 'internal_error', message: 'Directory unavailable' }, { status: 500 })
        ],
        {
          1: () => new Response(null, { status: 204 }),
          2: () => Response.json(
            { error: 'person_merge_active', message: "Split the person's active merge lineage before deleting this profile" },
            { status: 409 }
          )
        }
      );

      await deleteAllLoaded();

      expect(await screen.findByText('Directory unavailable')).toBeDefined();
      expect(await screen.findByText('1 deleted; 1 could not be deleted:')).toBeDefined();
      expect(screen.getByText(/active merge lineage before deleting this profile/)).toBeDefined();
    });

    it('shows the server message for each person that could not be deleted', async () => {
      renderDirectory(
        [
          { people: [person(1, 'Alpha Fixture'), person(2, 'Bravo Fixture')] },
          { people: [person(2, 'Bravo Fixture')] }
        ],
        {
          1: () => new Response(null, { status: 204 }),
          2: () => Response.json(
            { error: 'person_carddav_published', message: 'Unpublish this person from CardDAV before deleting it' },
            { status: 409 }
          )
        }
      );

      await deleteAllLoaded();

      const alert = await screen.findByRole('alert');
      expect(alert.textContent).toContain('1 deleted; 1 could not be deleted:');
      expect(alert.textContent).toContain('Bravo Fixture: Unpublish this person from CardDAV before deleting it');
      expect(alert.textContent).not.toContain('Reload the directory');
    });
  });

  it('opens a listed person in People through the navigation callback', async () => {
    const client = createAPIClient(async () => directoryResponse());
    const controller = new DirectoryController(client);
    const onOpenPerson = vi.fn();
    render(DirectoryWorkspace, { client, controller, state, onOpenPerson });
    await fireEvent.click(await screen.findByRole('row', { name: /Synthetic Person/ }));
    expect(onOpenPerson).toHaveBeenCalledExactlyOnceWith(7);
    controller.destroy();
  });
});
