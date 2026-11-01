import { fireEvent, render, screen, within, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import type { PersonNetwork as PersonNetworkProjection } from '../../directory/models';
import PersonNetwork from './PersonNetwork.svelte';

function requestOf(input: RequestInfo | URL): Request {
  return input instanceof Request ? input : new Request(input);
}

function rootOnly(depth = 1): PersonNetworkProjection {
  return {
    root_person_id: 7,
    depth,
    truncated: false,
    nodes: [{ id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 }],
    edges: []
  };
}

function connected(depth = 2, truncated = false): PersonNetworkProjection {
  return {
    root_person_id: 7,
    depth,
    truncated,
    nodes: [
      { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
      { id: 'person:8', kind: 'person', entity_id: 8, label: 'Curated Peer', hop: 1 },
      { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Shared Organization', hop: 2 },
      { id: 'person:9', kind: 'person', entity_id: 9, label: 'Second Hop Person', hop: 2 }
    ],
    edges: [
      { id: 'relationship:31', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', relationship_type_slug: 'colleague', label: 'works with' },
      { id: 'employment:41', kind: 'employment', source_node_id: 'person:8', target_node_id: 'organization:21', label: 'Engineer' },
      { id: 'relationship:32', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:9', relationship_type_slug: 'mentor', label: 'mentors' }
    ]
  };
}

function depthThree(): PersonNetworkProjection {
  return {
    root_person_id: 7,
    depth: 3,
    truncated: false,
    nodes: [
      { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
      { id: 'person:8', kind: 'person', entity_id: 8, label: 'First Hop Person', hop: 1 },
      { id: 'person:9', kind: 'person', entity_id: 9, label: 'Second Hop Person', hop: 2 },
      { id: 'person:10', kind: 'person', entity_id: 10, label: 'Depth Three Person', hop: 3 }
    ],
    edges: [
      { id: 'relationship:31', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', relationship_type_slug: 'knows', label: 'knows' },
      { id: 'relationship:32', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:9', relationship_type_slug: 'knows', label: 'knows' },
      { id: 'relationship:33', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:10', relationship_type_slug: 'knows', label: 'knows' }
    ]
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

describe('PersonNetwork', () => {
  it('keeps the third-step node inside the graph drawing', async () => {
    const client = createAPIClient(async (input) => Response.json(Number(new URL(requestOf(input).url).searchParams.get('depth')) === 3 ? depthThree() : rootOnly()));
    const controller = new DirectoryEntityController(client, 7);
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    const target = await screen.findByRole('button', { name: 'Open person Depth Three Person' });
    const node = target.closest<HTMLElement>('.node')!;
    const graph = node.closest<HTMLElement>('.graph')!;
    expect(parseFloat(graph.style.width)).toBeGreaterThanOrEqual(parseFloat(node.style.left) + parseFloat(node.style.maxWidth));
  });

  it('shows the empty state and no graph for a root-only projection', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(rootOnly())));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });

    expect(await screen.findByText('No connections at this depth')).toBeDefined();
    expect(screen.queryByRole('region', { name: 'Network graph' })).toBeNull();
    expect(screen.queryByRole('list', { name: 'Directory network connections' })).toBeNull();
  });

  it('draws every typed relationship and employment with its relation label and makes both entity kinds actionable', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(connected())));
    const controller = new DirectoryEntityController(client, 7);
    const onOpenPerson = vi.fn();
    const onOpenOrganization = vi.fn();

    render(PersonNetwork, { controller, onOpenPerson, onOpenOrganization });

    const peer = await screen.findByRole('button', { name: 'Open person Curated Peer' });
    const region = screen.getByRole('region', { name: 'Network graph' });
    expect(region.tabIndex).toBe(0);
    expect(document.querySelector('.graph > svg')?.getAttribute('aria-hidden')).toBe('true');
    expect(screen.queryByRole('list', { name: 'Directory network connections' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Open organization Shared Organization' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Open person Second Hop Person' })).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Open person Selected Person' })).toBeNull();
    const relationOf = (button: HTMLElement) => button.parentElement?.querySelector('.relation')?.textContent;
    expect(relationOf(peer)).toBe('Selected Person works with');
    expect(relationOf(screen.getByRole('button', { name: 'Open organization Shared Organization' }))).toBe('Engineer');
    expect(relationOf(screen.getByRole('button', { name: 'Open person Second Hop Person' }))).toBe('Curated Peer mentors');

    await fireEvent.click(peer);
    await fireEvent.click(screen.getByRole('button', { name: 'Open organization Shared Organization' }));
    expect(onOpenPerson).toHaveBeenCalledWith(8);
    expect(onOpenOrganization).toHaveBeenCalledWith(21);
  });

  it('names both ends of a link, in either direction and between two people at the same distance', async () => {
    const projection = connected();
    projection.nodes.push({ id: 'person:10', kind: 'person', entity_id: 10, label: 'Third Person', hop: 2 });
    projection.edges.push(
      { id: 'relationship:33', kind: 'relationship', source_node_id: 'person:10', target_node_id: 'person:8', relationship_type_slug: 'mentor', label: 'mentors' },
      { id: 'relationship:34', kind: 'relationship', source_node_id: 'person:10', target_node_id: 'person:9', relationship_type_slug: 'sibling', label: 'sibling of' },
    );
    const controller = new DirectoryEntityController(createAPIClient(vi.fn<typeof fetch>(async () => Response.json(projection))), 7);
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });

    const relationOf = async (name: string) => (await screen.findByRole('button', { name })).parentElement?.querySelector('.relation')?.textContent;
    // Curated Peer mentors Second Hop Person; Third Person mentors Curated Peer. The same verb reads differently by direction.
    expect(await relationOf('Open person Second Hop Person')).toBe('Curated Peer mentors, Third Person sibling of');
    expect(await relationOf('Open person Third Person')).toBe('mentors Curated Peer');
    const links = within(screen.getByRole('list', { name: 'Network connections' })).getAllByRole('listitem').map((item) => item.textContent);
    expect(links).toContain('Third Person sibling of Second Hop Person');
    expect(links).toHaveLength(5);
  });

  it('sends exact depth and ended-data queries and retains the last projection through loading, error, and retry', async () => {
    const depthTwo = deferredResponse();
    const requests: Request[] = [];
    let depthTwoAttempts = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const url = new URL(request.url);
      const depth = Number(url.searchParams.get('depth'));
      const includeEnded = url.searchParams.get('include_ended') === 'true';
      if (depth === 1) return Response.json(connected(1));
      if (depth === 2 && !includeEnded) return depthTwo.promise;
      if (depth === 2 && includeEnded) {
        depthTwoAttempts += 1;
        return depthTwoAttempts === 1
          ? Response.json({ error: 'unavailable', message: 'Synthetic network unavailable.' }, { status: 503 })
          : Response.json(connected(2));
      }
      return Response.json(connected(depth));
    }));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    expect((await screen.findAllByText('Curated Peer')).length).toBeGreaterThan(0);

    await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
    expect((await screen.findByRole('status')).textContent).toContain('Loading network');
    expect(screen.getAllByText('Curated Peer').length).toBeGreaterThan(0);
    depthTwo.resolve(Response.json(connected(2)));
    await waitFor(() => expect(screen.queryByRole('status')).toBeNull());

    await fireEvent.click(screen.getByRole('checkbox', { name: 'Include ended connections' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic network unavailable.');
    expect(screen.getAllByText('Curated Peer').length).toBeGreaterThan(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry network' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());

    const queries = requests.map((request) => new URL(request.url).searchParams);
    expect(queries.some((query) => query.get('depth') === '1' && query.get('include_ended') === 'false')).toBe(true);
    expect(queries.some((query) => query.get('depth') === '2' && query.get('include_ended') === 'false')).toBe(true);
    expect(queries.filter((query) => query.get('depth') === '2' && query.get('include_ended') === 'true')).toHaveLength(2);

    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    await waitFor(() => expect(requests.some((request) => new URL(request.url).searchParams.get('depth') === '3')).toBe(true));
  });

  it('discards a stale depth response and describes truncation as an at-most bounded prefix', async () => {
    const depthTwo = deferredResponse();
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const depth = Number(new URL(request.url).searchParams.get('depth'));
      if (depth === 2) return depthTwo.promise;
      return Response.json(depth === 3 ? connected(3, true) : rootOnly(1));
    }));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByText('No connections at this depth');
    await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
    await waitFor(() => expect(requests.some((request) => new URL(request.url).searchParams.get('depth') === '2')).toBe(true));
    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    expect((await screen.findAllByText('Curated Peer')).length).toBeGreaterThan(0);
    depthTwo.resolve(Response.json(connected(2)));
    await waitFor(() => expect(screen.getByText(/bounded prefix/).textContent).toContain('at most 250 nodes and 500 connections at depth 3'));
    expect(screen.getByRole('button', { name: 'Open person Second Hop Person' })).toBeDefined();
  });
});
