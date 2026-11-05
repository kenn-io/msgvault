import { controllerWith, requestOf } from '../../directory/entity-fixtures.test-support';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import type { PersonNetwork as PersonNetworkProjection } from '../../directory/models';
import PersonNetwork from './PersonNetwork.svelte';

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
  const network: PersonNetworkProjection = {
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
  network.nodes = network.nodes.filter((node) => node.hop <= depth);
  const ids = new Set(network.nodes.map((node) => node.id));
  network.edges = network.edges.filter((edge) => ids.has(edge.source_node_id) && ids.has(edge.target_node_id));
  return network;
}

function depthThree(): PersonNetworkProjection {
  return {
    root_person_id: 7,
    depth: 3,
    truncated: false,
    nodes: [
      { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
      { id: 'person:8', kind: 'person', entity_id: 8, label: 'Curated Peer', hop: 1 },
      { id: 'person:9', kind: 'person', entity_id: 9, label: 'Second Hop Person', hop: 2 },
      { id: 'person:10', kind: 'person', entity_id: 10, label: 'Depth Three Person', hop: 3 }
    ],
    edges: [
      { id: 'relationship:31', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', relationship_type_slug: 'colleague', label: 'works with' },
      { id: 'relationship:32', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:9', relationship_type_slug: 'mentor', label: 'mentors' },
      { id: 'relationship:33', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:10', relationship_type_slug: 'knows', label: 'knows' }
    ]
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

function networkFetcher(network: PersonNetworkProjection) {
  return vi.fn<typeof fetch>(async () => Response.json(network));
}

describe('PersonNetwork', () => {
  it('draws every connection with a chip and opens records', async () => {
    const { controller } = controllerWith(networkFetcher(connected()));
    const onOpenPerson = vi.fn();
    const onOpenOrganization = vi.fn();
    const { container } = render(PersonNetwork, { controller, onOpenPerson, onOpenOrganization });

    const list = await screen.findByRole('list', { name: 'Network connections' });
    expect([...list.querySelectorAll('li')].map((item) => item.textContent)).toEqual([
      'Selected Person · works with · Curated Peer',
      'Curated Peer · Engineer · Shared Organization',
      'Curated Peer · mentors · Second Hop Person'
    ]);
    const paths = container.querySelectorAll('path');
    expect(paths).toHaveLength(3);
    expect(container.querySelectorAll('.edge-label')).toHaveLength(3);
    const cross = paths[0]!.getAttribute('d')!.split(' ');
    const chip = [...container.querySelectorAll<HTMLElement>('.edge-label')].find((chip) => chip.textContent === 'works with')!;
    expect(parseFloat(chip.style.left)).toBe((Number(cross[1]) + Number(cross[4])) / 2);
    expect(parseFloat(chip.style.top)).toBe((Number(cross[2]) + Number(cross[5])) / 2);
    expect([...container.querySelectorAll('.edge-label')].find((chip) => chip.textContent === 'Engineer')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Curated Peer' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Shared Organization' }));
    expect(onOpenPerson).toHaveBeenCalledWith(8);
    expect(onOpenOrganization).toHaveBeenCalledWith(21);
    expect(screen.queryByRole('button', { name: 'Selected Person' })).toBeNull();
    expect(screen.getByRole('region', { name: 'Network graph' }).tabIndex).toBe(0);
  });

  it('marks an incoming relationship label right to left', async () => {
    const network = connected();
    network.nodes = network.nodes.slice(0, 2);
    network.edges = [{ id: 'relationship:31', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:7', relationship_type_slug: 'mentor', label: 'mentors' }];
    const { controller } = controllerWith(networkFetcher(network));
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Curated Peer' });
    expect(container.querySelector('.edge-label')?.textContent).toBe('← mentors');
  });

  it('marks an upward same-column relationship label bottom to top', async () => {
    const network: PersonNetworkProjection = {
      root_person_id: 7, depth: 2, truncated: false,
      nodes: [
        { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
        { id: 'person:8', kind: 'person', entity_id: 8, label: 'Alpha', hop: 1 },
        { id: 'person:9', kind: 'person', entity_id: 9, label: 'Beta', hop: 1 }
      ],
      edges: [
        { id: 'relationship:31', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', label: 'knows' },
        { id: 'relationship:32', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:9', label: 'knows' },
        { id: 'relationship:33', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:8', relationship_type_slug: 'mentor', label: 'mentors' }
      ]
    };
    const { controller } = controllerWith(networkFetcher(network));
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Beta' });
    expect([...container.querySelectorAll('.edge-label')].map((chip) => chip.textContent)).toContain('↑ mentors');
  });

  it('joins three relations between direct people in reading order', async () => {
    const network = connected();
    network.nodes = network.nodes.slice(0, 2);
    network.edges = [network.edges[0]!, { id: 'relationship:35', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', label: 'knows' }];
    network.edges.push({ id: 'relationship:34', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:7', relationship_type_slug: 'mentor', label: 'mentors' });
    const { controller } = controllerWith(networkFetcher(network));
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Curated Peer' });
    expect(container.querySelector('.edge-label')?.textContent).toBe('works with · knows · ← mentors');
    expect(container.querySelectorAll('.edge-label')).toHaveLength(1);
    expect(screen.getByRole('list', { name: 'Network connections' }).querySelectorAll('li')).toHaveLength(3);
  });

  it('hides a root-only direct network', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json(rootOnly()));
    const controller = new DirectoryEntityController(createAPIClient(fetcher), 7);
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await waitFor(() => expect(controller.network).not.toBeNull());
    expect(screen.queryByRole('heading', { name: 'Network' })).toBeNull();
  });

  it('shows a failed first load and retries even without an earlier graph', async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({ error: 'unavailable', message: 'Synthetic network unavailable.' }, { status: 503 }))
      .mockResolvedValueOnce(Response.json(connected()));
    const controller = new DirectoryEntityController(createAPIClient(fetcher), 7);
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    expect((await screen.findByRole('alert')).textContent).toContain('Synthetic network unavailable.');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry network' }));
    await screen.findByRole('button', { name: 'Curated Peer' });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('sends exact depth and ended-data queries and retains the last projection through loading, error, and retry', async () => {
    const depthTwo = deferredResponse();
    const requests: Request[] = [];
    let depthTwoAttempts = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (new URL(request.url).pathname.endsWith('/relationships')) return Response.json({ relationships: [] });
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
      return Response.json(depthThree());
    }));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    expect((await screen.findAllByText('Curated Peer')).length).toBeGreaterThan(0);

    await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
    expect((await screen.findByRole('status')).textContent).toContain('Loading network');
    expect(screen.getAllByText('Curated Peer').length).toBeGreaterThan(0);
    depthTwo.resolve(Response.json(connected(2)));
    await waitFor(() => expect(screen.queryByRole('status')).toBeNull());

    await controller.refreshRelationships(true);
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
    await screen.findByRole('button', { name: 'Depth Three Person' });
    expect(screen.getByRole('list', { name: 'Network connections' }).textContent).toContain('Depth Three Person');
  });
});
