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

function connected(depth = 2): PersonNetworkProjection {
  const network: PersonNetworkProjection = {
    root_person_id: 7,
    depth,
    truncated: false,
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
  return vi.fn<typeof fetch>(async (input) => {
    const depth = Number(new URL(requestOf(input).url).searchParams.get('depth'));
    const nodes = network.nodes.filter((node) => node.hop <= depth);
    const hops = new Map(nodes.map((node) => [node.id, node.hop]));
    const edges = network.edges.filter((edge) => hops.has(edge.source_node_id) && hops.has(edge.target_node_id) && Math.min(hops.get(edge.source_node_id)!, hops.get(edge.target_node_id)!) < depth);
    return Response.json({ ...network, depth, nodes, edges });
  });
}

const mentor = {
  id: 31, revision: 1, slug: 'mentor', forward_label: 'Mentor', reverse_label: 'Mentee',
  is_symmetric: false, is_canonical: false, is_deletable: true, ownership: 'user' as const, universal_id: 'relationship-type-31',
  created_at: '2026-08-01T00:00:00Z', updated_at: '2026-08-01T00:00:00Z'
};

function rows(region: HTMLElement): string[] {
  return [...region.querySelectorAll('.detail-row')].map((row) => row.textContent!.trim());
}

describe('PersonNetwork', () => {
  it.each([false, true])('lists reached people and opens records, truncated=%s', async truncated => {
    const network = connected();
    network.truncated = truncated;
    if (truncated) {
      network.nodes.push({ id: 'person:11', kind: 'person', entity_id: 11, label: 'Other Peer', hop: 1 }, { id: 'person:12', kind: 'person', entity_id: 12, label: 'Third Peer', hop: 1 });
      network.edges.push(...[11, 12].flatMap(id => [
        { id: `root:${id}`, kind: 'relationship' as const, source_node_id: 'person:7', target_node_id: `person:${id}`, label: 'Friend' },
        { id: `job:${id}`, kind: 'employment' as const, source_node_id: `person:${id}`, target_node_id: 'organization:21', label: 'Engineer' },
      ]));
    }
    const { controller } = controllerWith(networkFetcher(network));
    controller.relationshipTypes = [mentor];
    const onOpenPerson = vi.fn();
    const onOpenOrganization = vi.fn();
    render(PersonNetwork, { controller, onOpenPerson, onOpenOrganization });

    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Shared Organization' });
    const region = screen.getByRole('region', { name: 'Network at 2 steps' });
    // Curated Peer is a direct link with no link to another direct one, which Connections already lists, so it shows only as the way through.
    expect(rows(region)).toEqual(['Second Hop PersonMentee · via Curated Peer', truncated ? 'Shared OrganizationEngineer · via Curated Peer, Other Peer, and Third Peer' : 'Shared OrganizationEngineer · via Curated Peer']);
    if (truncated) expect(screen.getByText('Showing up to 250 people and organizations and 500 connections.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Curated Peer' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Selected Person' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Second Hop Person' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Shared Organization' }));
    expect(onOpenPerson).toHaveBeenCalledWith(9);
    expect(onOpenOrganization).toHaveBeenCalledWith(21);
  });

  it('names a person reached through an organization by their job there', async () => {
    const network: PersonNetworkProjection = {
      root_person_id: 7, depth: 2, truncated: false,
      nodes: [
        { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
        { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Cedar Circle', hop: 1 },
        { id: 'organization:22', kind: 'organization', entity_id: 22, label: 'Lumen Grove', hop: 1 },
        { id: 'person:9', kind: 'person', entity_id: 9, label: 'Sela Moss', hop: 2 }
      ],
      edges: [
        { id: 'employment:1', kind: 'employment', source_node_id: 'person:7', target_node_id: 'organization:21', label: 'Workshop volunteer' },
        { id: 'employment:2', kind: 'employment', source_node_id: 'person:7', target_node_id: 'organization:22', label: 'Garden designer' },
        { id: 'employment:3', kind: 'employment', source_node_id: 'person:9', target_node_id: 'organization:21', label: 'Workshop lead' },
        { id: 'employment:4', kind: 'employment', source_node_id: 'person:9', target_node_id: 'organization:22', label: 'Advisor' }
      ]
    };
    render(PersonNetwork, { controller: controllerWith(networkFetcher(network)).controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Sela Moss' });
    expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toEqual(['Sela MossWorkshop lead · via Cedar Circle; Advisor · via Lumen Grove']);
  });

  it('keeps links between direct contacts at two steps', async () => {
    const network = connected();
    network.nodes.push({ id: 'person:11', kind: 'person', entity_id: 11, label: 'Other Peer', hop: 1 }, { id: 'organization:22', kind: 'organization', entity_id: 22, label: 'Own Organization', hop: 1 });
    network.edges.push(
      { id: 'relationship:34', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:11', relationship_type_slug: 'friend', label: 'Friend' },
      { id: 'employment:42', kind: 'employment', source_node_id: 'person:7', target_node_id: 'organization:22', label: 'Designer' },
      { id: 'relationship:35', kind: 'relationship', source_node_id: 'person:11', target_node_id: 'person:8', relationship_type_slug: 'mentor', label: 'Mentor' },
      { id: 'employment:43', kind: 'employment', source_node_id: 'person:8', target_node_id: 'organization:22', label: 'Advisor' }
    );
    const { controller } = controllerWith(networkFetcher(network));
    controller.relationshipTypes = [mentor];
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Second Hop Person' });
    expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toEqual([
      'Curated PeerAdvisor · with Own Organization',
      'Other PeerMentor · with Curated Peer',
      'Second Hop PersonMentee · via Curated Peer',
      'Shared OrganizationEngineer · via Curated Peer'
    ]);
  });

  it('says a link between two at the same step once, on the side it describes', async () => {
    const network: PersonNetworkProjection = {
      root_person_id: 7, depth: 3, truncated: false,
      nodes: [
        { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
        { id: 'person:8', kind: 'person', entity_id: 8, label: 'Direct Friend', hop: 1 },
        { id: 'person:9', kind: 'person', entity_id: 9, label: 'Cedar Bloom', hop: 2 },
        { id: 'person:10', kind: 'person', entity_id: 10, label: 'Maple Reed', hop: 2 },
        { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Fern Hollow', hop: 2 }
      ],
      edges: [
        { id: 'relationship:1', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', relationship_type_slug: 'friend', label: 'Friend' },
        { id: 'relationship:2', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:9', relationship_type_slug: 'friend', label: 'Friend' },
        { id: 'relationship:3', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:10', relationship_type_slug: 'friend', label: 'Friend' },
        { id: 'employment:1', kind: 'employment', source_node_id: 'person:8', target_node_id: 'organization:21', label: 'Advisor' },
        { id: 'relationship:4', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:10', relationship_type_slug: 'mentor', label: 'Mentor' },
        { id: 'employment:2', kind: 'employment', source_node_id: 'person:10', target_node_id: 'organization:21', label: 'Gardener' }
      ]
    };
    const { controller } = controllerWith(networkFetcher(network));
    controller.relationshipTypes = [mentor];
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Maple Reed' });
    // At 2 steps the links among the second step are past the edge of what was loaded.
    expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toEqual([
      'Cedar BloomFrom Direct Friend: Friend',
      'Fern HollowAdvisor · via Direct Friend',
      'Maple ReedFrom Direct Friend: Friend'
    ]);
    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    await waitFor(() => expect(rows(screen.getByRole('region', { name: 'Network at 3 steps' }))).toEqual([
      'Cedar BloomFrom Direct Friend: Friend; Mentor · with Maple Reed',
      'Fern HollowAdvisor · via Direct Friend',
      'Maple ReedFrom Direct Friend: Friend; Gardener · with Fern Hollow'
    ]));
  });

  it('opens on Direct as one line naming the people reached directly', async () => {
    const fetcher = networkFetcher(connected());
    render(PersonNetwork, { controller: controllerWith(fetcher).controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByText('Curated Peer');
    expect(new URL(requestOf(fetcher.mock.calls[0]![0]).url).searchParams.get('depth')).toBe('1');
    expect(screen.queryByRole('region', { name: 'Network at 2 steps' })).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it.each([
    { labels: ['Lumen Grove'], expected: 'Curated Peer and Lumen Grove', checkEmpty: true },
    { labels: ['Lumen Grove', 'Cedar Circle'], expected: 'Curated Peer, Lumen Grove, and Cedar Circle', checkEmpty: false },
    { labels: ['Lumen Grove', 'Cedar Circle', 'Fern Hollow', 'Moss Yard'], expected: 'Curated Peer, Lumen Grove, and 3 more', checkEmpty: false }
  ])('summarizes Direct as $expected', async ({ labels, expected, checkEmpty }) => {
    const network = connected(1);
    labels.forEach((label, index) => {
      const id = 22 + index;
      network.nodes.push({ id: `organization:${id}`, kind: 'organization', entity_id: id, label, hop: 1 });
      network.edges.push({ id: `employment:${id}`, kind: 'employment', source_node_id: 'person:7', target_node_id: `organization:${id}`, label: 'Designer' });
    });
    render(PersonNetwork, { controller: controllerWith(networkFetcher(network)).controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByText(expected);
    if (checkEmpty) {
      await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
      await screen.findByText('No one else at 2 steps');
      expect(screen.queryByRole('button', { name: 'Lumen Grove' })).toBeNull();
      await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
      await screen.findByText('No one else at 3 steps');
    }
  });

  it('reads a link from the far end when it runs toward the direct person', async () => {
    const network = connected();
    network.nodes = [network.nodes[0]!, network.nodes[1]!, network.nodes[3]!];
    network.edges = [network.edges[0]!, { id: 'relationship:32', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:8', relationship_type_slug: 'mentor', label: 'Mentor' }];
    const { controller } = controllerWith(networkFetcher(network));
    controller.relationshipTypes = [mentor];
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Second Hop Person' });
    expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toEqual(['Second Hop PersonMentor · via Curated Peer']);
  });

  it('hides a root-only direct network', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json(rootOnly()));
    const controller = new DirectoryEntityController(createAPIClient(fetcher), 7);
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await waitFor(() => expect(controller.network).not.toBeNull());
    expect(screen.queryByRole('heading', { name: 'Network' })).toBeNull();
  });

  it('shows a failed first load in the failure voice, with the server message in Details, and retries', async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json({ error: 'unavailable', message: 'Synthetic network unavailable.' }, { status: 503 }))
      .mockResolvedValueOnce(Response.json(connected(1)));
    const controller = new DirectoryEntityController(createAPIClient(fetcher), 7);
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't load the network.");
    expect(container.querySelector('details pre')?.textContent).toBe('Synthetic network unavailable.');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Curated Peer');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('reloads for depth and keeps the list through loading, error, and retry', async () => {
    const depthThreeLoad = deferredResponse();
    let depthThreeAttempts = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (new URL(request.url).pathname.endsWith('/relationships')) return Response.json({ relationships: [] });
      const url = new URL(request.url);
      const depth = Number(url.searchParams.get('depth'));
      if (depth === 1) return Response.json(connected(1));
      if (depth === 2) return Response.json(connected(2));
      depthThreeAttempts += 1;
      return depthThreeAttempts === 1 ? depthThreeLoad.promise : Response.json(depthThree());
    }));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByText('Curated Peer');
    await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Second Hop Person' });

    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    await waitFor(() => expect(screen.getByRole('region', { name: 'Network at 3 steps' }).getAttribute('aria-busy')).toBe('true'));
    expect(screen.getByRole('button', { name: 'Second Hop Person' })).toBeDefined();
    depthThreeLoad.resolve(Response.json({ error: 'unavailable', message: 'Synthetic network unavailable.' }, { status: 503 }));
    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't load the network.");
    expect(screen.getByRole('button', { name: 'Second Hop Person' })).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
    await screen.findByRole('button', { name: 'Depth Three Person' });
    expect(rows(screen.getByRole('region', { name: 'Network at 3 steps' }))).toEqual(['Second Hop PersonFrom Curated Peer: mentors', 'Depth Three PersonFrom Second Hop Person: knows']);
  });

  it('keeps a forward edge label without a relationship catalog', async () => {
    const network = connected();
    network.edges[2] = { ...network.edges[2]!, source_node_id: 'person:9', target_node_id: 'person:8' };
    const { controller } = controllerWith(networkFetcher(network));
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    try {
      await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
      await screen.findByRole('button', { name: 'Second Hop Person' });
      expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toContain('Second Hop PersonMentors · via Curated Peer');
    } finally { controller.destroy(); }
  });
});
