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
  it.each([
    { jobs: ['Workshop lead', 'Advisor'], expected: 'Sela MossWorkshop lead · via Cedar Circle; Advisor · via Lumen Grove' },
    { jobs: ['Workshop lead', 'Workshop lead', 'Workshop lead'], expected: 'Sela MossWorkshop lead · via Cedar Circle, Lumen Grove, and Fern Hollow' }
  ])('names a person reached through an organization by their job there: $expected', async ({ jobs, expected }) => {
    const organizations = ['Cedar Circle', 'Lumen Grove', 'Fern Hollow'].slice(0, jobs.length).map((label, index) => ({ id: `organization:${21 + index}`, kind: 'organization' as const, entity_id: 21 + index, label, hop: 1 }));
    const network: PersonNetworkProjection = {
      root_person_id: 7, depth: 2, truncated: false,
      nodes: [
        { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
        ...organizations,
        { id: 'person:9', kind: 'person', entity_id: 9, label: 'Sela Moss', hop: 2 }
      ],
      edges: organizations.flatMap((organization, index) => [
        { id: `root:${index}`, kind: 'employment' as const, source_node_id: 'person:7', target_node_id: organization.id, label: 'Volunteer' },
        { id: `job:${index}`, kind: 'employment' as const, source_node_id: 'person:9', target_node_id: organization.id, label: jobs[index]! }
      ])
    };
    render(PersonNetwork, { controller: controllerWith(networkFetcher(network)).controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Sela Moss' });
    expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toEqual([expected]);
  });

  it.each([
    {
      name: 'links between direct contacts appear at two steps',
      network: {
        root_person_id: 7, depth: 3, truncated: false,
        nodes: [
          { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
          { id: 'person:9', kind: 'person', entity_id: 9, label: 'Cedar Bloom', hop: 1 },
          { id: 'person:10', kind: 'person', entity_id: 10, label: 'Maple Reed', hop: 1 },
          { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Fern Hollow', hop: 1 }
        ],
        edges: [
          { id: 'relationship:2', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:9', relationship_type_slug: 'friend', label: 'Friend' },
          { id: 'relationship:3', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:10', relationship_type_slug: 'friend', label: 'Friend' },
          { id: 'employment:1', kind: 'employment', source_node_id: 'person:7', target_node_id: 'organization:21', label: 'Advisor' },
          { id: 'relationship:4', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:10', relationship_type_slug: 'mentor', label: 'Mentor' },
          { id: 'employment:2', kind: 'employment', source_node_id: 'person:10', target_node_id: 'organization:21', label: 'Gardener' }
        ]
      } satisfies PersonNetworkProjection,
      levels: [{ depth: 2, expected: ['Cedar BloomMentor · with Maple Reed', 'Maple ReedGardener · with Fern Hollow'] }]
    },
    {
      name: 'links between second-step contacts appear at three steps',
      network: {
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
      } satisfies PersonNetworkProjection,
      levels: [
        { depth: 2, expected: ['Cedar BloomFrom Direct Friend: Friend', 'Fern HollowAdvisor · via Direct Friend', 'Maple ReedFrom Direct Friend: Friend'] },
        { depth: 3, expected: ['Cedar BloomFrom Direct Friend: Friend; Mentor · with Maple Reed', 'Fern HollowAdvisor · via Direct Friend', 'Maple ReedFrom Direct Friend: Friend; Gardener · with Fern Hollow'] }
      ]
    }
  ])('shows same-step links once, on the side they describe: $name', async ({ network, levels }) => {
    const { controller } = controllerWith(networkFetcher(network));
    controller.relationshipTypes = [mentor];
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    for (const { depth, expected } of levels) {
      await fireEvent.click(await screen.findByRole('radio', { name: `${depth} steps` }));
      await waitFor(() => expect(rows(screen.getByRole('region', { name: `Network at ${depth} steps` }))).toEqual(expected));
    }
  });

  it.each([
    { name: 'Curated Peer and Lumen Grove', network: connected(1), labels: ['Lumen Grove'], expected: 'Curated Peer and Lumen Grove', links: ['Curated Peer', 'Lumen Grove'], people: [[8]], organizations: [[22]] },
    { name: 'Lumen Grove, Cedar Circle, and 4 more', network: rootOnly(), labels: ['Lumen Grove', 'Cedar Circle', 'Fern Hollow', 'Moss Yard', 'Willow Field', 'Birch Garden'], expected: 'Lumen Grove, Cedar Circle, and 4 more', links: ['Lumen Grove', 'Cedar Circle'], people: [], organizations: [[22], [23]] }
  ])('summarizes Direct: $name', async ({ network, labels, expected, links, people, organizations }) => {
    labels.forEach((label, index) => {
      const id = 22 + index;
      network.nodes.push({ id: `organization:${id}`, kind: 'organization', entity_id: id, label, hop: 1 });
      network.edges.push({ id: `employment:${id}`, kind: 'employment', source_node_id: 'person:7', target_node_id: `organization:${id}`, label: 'Designer' });
    });
    const fetcher = networkFetcher(network);
    const onOpenPerson = vi.fn();
    const onOpenOrganization = vi.fn();
    const { controller } = controllerWith(fetcher);
    const { container } = render(PersonNetwork, { controller, onOpenPerson, onOpenOrganization });
    await waitFor(() => {
      expect(controller.network?.root_person_id).toBe(7);
      expect(controller.networkLoading).toBe(false);
    });
    expect(new URL(requestOf(fetcher.mock.calls[0]![0]).url).searchParams.get('depth')).toBe('1');
    expect(screen.queryByRole('region', { name: 'Network at 2 steps' })).toBeNull();
    expect(container.querySelector('.summary')?.textContent?.trim()).toBe(expected);
    for (const name of links) await fireEvent.click(screen.getByRole('button', { name }));
    expect(onOpenPerson.mock.calls).toEqual(people);
    expect(onOpenOrganization.mock.calls).toEqual(organizations);
  });

  it.each([
    { catalog: [mentor], reverse: false, expected: 'Second Hop PersonMentor · via Curated Peer' },
    { catalog: [], reverse: false, expected: 'Second Hop PersonMentors · via Curated Peer' },
    { catalog: [mentor], reverse: true, expected: 'Second Hop PersonMentee · via Curated Peer' }
  ])('reads a link from either end: $expected', async ({ catalog, reverse, expected }) => {
    const network = connected();
    if (!reverse) network.edges[2] = { ...network.edges[2]!, source_node_id: 'person:9', target_node_id: 'person:8' };
    const { controller } = controllerWith(networkFetcher(network));
    controller.relationshipTypes = catalog;
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await fireEvent.click(await screen.findByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Second Hop Person' });
    expect(rows(screen.getByRole('region', { name: 'Network at 2 steps' }))).toEqual([expected, 'Shared OrganizationEngineer · via Curated Peer']);
    expect(screen.queryByRole('button', { name: 'Curated Peer' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Selected Person' })).toBeNull();
  });

  it.each([
    { name: 'root-only Direct is hidden', network: rootOnly(), sections: 0, notices: 0, emptyDepths: [], directLinks: [] },
    { name: 'a truncated Direct result is reported', network: { ...connected(1), truncated: true }, sections: 1, notices: 1, emptyDepths: [], directLinks: [] },
    { name: 'direct-only contacts leave 2 and 3 steps empty', network: connected(1), sections: 1, notices: 0, emptyDepths: [2, 3], directLinks: ['Curated Peer'] }
  ])('shows network result state: $name', async ({ network, sections, notices, emptyDepths, directLinks }) => {
    const { controller } = controllerWith(networkFetcher(network));
    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await waitFor(() => {
      expect(controller.network?.root_person_id).toBe(7);
      expect(controller.networkLoading).toBe(false);
    });
    expect(screen.queryAllByRole('heading', { name: 'Network' })).toHaveLength(sections);
    expect(screen.queryAllByText('Showing up to 250 people and organizations and 500 connections.')).toHaveLength(notices);
    for (const name of directLinks) expect(screen.getByRole('button', { name })).toBeDefined();
    for (const depth of emptyDepths) {
      await fireEvent.click(screen.getByRole('radio', { name: `${depth} steps` }));
      await screen.findByText(`No one else at ${depth} steps`);
      for (const name of directLinks) expect(screen.queryByRole('button', { name })).toBeNull();
    }
  });

  it('retries a failed first load and keeps the list visible during depth loading', async () => {
    const depthThreeLoad = deferredResponse();
    let directAttempts = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      if (new URL(request.url).pathname.endsWith('/relationships')) return Response.json({ relationships: [] });
      const url = new URL(request.url);
      const depth = Number(url.searchParams.get('depth'));
      if (depth === 1) return ++directAttempts === 1 ? Response.json({ message: 'Synthetic network unavailable.' }, { status: 503 }) : Response.json(connected(1));
      if (depth === 2) return Response.json(connected(2));
      return depthThreeLoad.promise;
    }));
    const controller = new DirectoryEntityController(client, 7);

    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    expect((await screen.findByRole('alert')).textContent).toContain("Couldn't load the network.");
    expect(container.querySelector('details pre')?.textContent).toBe('Synthetic network unavailable.');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Curated Peer');
    await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
    await screen.findByRole('button', { name: 'Second Hop Person' });

    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    await waitFor(() => expect(screen.getByRole('region', { name: 'Network at 3 steps' }).getAttribute('aria-busy')).toBe('true'));
    expect(screen.getByRole('button', { name: 'Second Hop Person' })).toBeDefined();
    depthThreeLoad.resolve(Response.json(depthThree()));
    await screen.findByRole('button', { name: 'Depth Three Person' });
  });

});
