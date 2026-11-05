import { requestOf } from '../../directory/entity-fixtures.test-support';
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
  return {
    root_person_id: 7,
    depth,
    truncated,
    nodes: [
      { id: 'person:7', kind: 'person', entity_id: 7, label: 'Selected Person', hop: 0 },
      { id: 'person:8', kind: 'person', entity_id: 8, label: 'Curated Peer', hop: 1 },
      { id: 'organization:21', kind: 'organization', entity_id: 21, label: 'Shared Organization', hop: 1 },
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
      { id: 'person:10', kind: 'person', entity_id: 10, label: 'Depth Three Person', hop: 3 }
    ],
    edges: [
      { id: 'relationship:33', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:10', relationship_type_slug: 'knows', label: 'knows' }
    ]
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((next) => { resolve = next; });
  return { promise, resolve };
}

const relationshipTypes = [
  { slug: 'colleague', forward_label: 'works with', reverse_label: 'works with', is_symmetric: true },
  { slug: 'mentor', forward_label: 'mentors', reverse_label: 'mentored by', is_symmetric: false }
].map((type, index) => ({ ...type, id: index + 1, revision: 1, ownership: 'system', is_canonical: true, is_deletable: false, universal_id: `urn:example:${type.slug}`, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' }));

function networkController(network: PersonNetworkProjection) {
  return new DirectoryEntityController(createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const path = new URL(requestOf(input).url).pathname;
    return Response.json(path === '/api/v1/relationship-types' ? { relationship_types: relationshipTypes } : network);
  })), network.root_person_id);
}

describe('PersonNetwork', () => {
  it('draws every connection with a chip and opens records', async () => {
    const controller = networkController(connected());
    await controller.refreshRelationshipTypes();
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
    const chip = container.querySelector<HTMLElement>('.edge-label[title="works with"]')!;
    expect(parseFloat(chip.style.left)).toBe((Number(cross[1]) + Number(cross[4])) / 2);
    expect(parseFloat(chip.style.top)).toBe((Number(cross[2]) + Number(cross[5])) / 2);
    expect(paths[1]!.getAttribute('d')).toBe('M 280 14 H 272 V 70 H 280');
    expect(container.querySelector('.edge-label[title="Engineer"]')).not.toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Curated Peer' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Shared Organization' }));
    expect(onOpenPerson).toHaveBeenCalledWith(8);
    expect(onOpenOrganization).toHaveBeenCalledWith(21);
    expect(screen.queryByRole('button', { name: 'Selected Person' })).toBeNull();
    expect(screen.getByRole('region', { name: 'Network graph' }).tabIndex).toBe(0);
  });

  it.each([
    ['mentor', false, 'mentors'],
    ['mentor', true, 'mentored by'],
    ['colleague', false, 'works with'],
    ['colleague', true, 'works with']
  ] as const)('orients %s with incoming=%s as %s', async (slug, incoming, expected) => {
    const network = connected();
    network.nodes = network.nodes.slice(0, 2);
    network.edges = [{ id: 'relationship:31', kind: 'relationship', source_node_id: incoming ? 'person:8' : 'person:7', target_node_id: incoming ? 'person:7' : 'person:8', relationship_type_slug: slug, label: slug === 'mentor' ? 'mentors' : 'works with' }];
    const controller = networkController(network);
    await controller.refreshRelationshipTypes();
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Curated Peer' });
    expect(container.querySelector('.edge-label > span')?.textContent).toBe(expected);
    expect(screen.getByRole('list', { name: 'Network connections' }).textContent).toContain(incoming ? 'Curated Peer ·' : 'Selected Person ·');
  });

  it('stacks distinct relationship types between one pair, each in reading order', async () => {
    const network = connected();
    network.edges.push({ id: 'relationship:34', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:7', relationship_type_slug: 'mentor', label: 'mentors' });
    const controller = networkController(network);
    await controller.refreshRelationshipTypes();
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Curated Peer' });
    const chip = container.querySelector('.edge-label[title="works with · mentored by"]')!;
    expect([...chip.children].map((row) => row.textContent)).toEqual(['works with', 'mentored by']);
    expect(container.querySelectorAll('.edge-label')).toHaveLength(3);
    expect(screen.getByRole('list', { name: 'Network connections' }).querySelectorAll('li')).toHaveLength(4);
  });

  it('slides crossing chips apart along their own lines', async () => {
    const network = connected();
    network.nodes = [network.nodes[0]!, ...[8, 9, 10, 11].map((id, index) => ({ id: `person:${id}`, kind: 'person' as const, entity_id: id, label: `Peer ${index}`, hop: index < 2 ? 1 : 2 }))];
    network.edges = [
      { id: 'r1', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:8', label: 'knows' },
      { id: 'r2', kind: 'relationship', source_node_id: 'person:7', target_node_id: 'person:9', label: 'knows' },
      { id: 'r3', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:10', label: 'knows' },
      { id: 'r4', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:11', label: 'guides' },
      { id: 'r5', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:10', label: 'parent of' },
      { id: 'r6', kind: 'relationship', source_node_id: 'person:9', target_node_id: 'person:11', label: 'knows' }
    ];
    const { container } = render(PersonNetwork, { controller: networkController(network), onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Peer 3' });
    const first = container.querySelector<HTMLElement>('.edge-label[title="guides"]')!;
    const second = container.querySelector<HTMLElement>('.edge-label[title="parent of"]')!;
    const x = parseFloat(second.style.left), y = parseFloat(second.style.top);
    expect([x, y]).not.toEqual([parseFloat(first.style.left), parseFloat(first.style.top)]);
    // The upward diagonal runs from (316, 70) to (552, 14).
    expect((x - 316) / 236).toBeCloseTo((70 - y) / 56);
    expect(Math.abs(x - parseFloat(first.style.left))).toBeGreaterThan(60);
  });

  it('routes an even-span same-column edge beside the middle node and slides its chip clear', async () => {
    const network = connected();
    network.nodes = [network.nodes[0]!, ...[8, 9, 10].map((id, index) => ({ id: `person:${id}`, kind: 'person' as const, entity_id: id, label: `Peer ${index}`, hop: 1 }))];
    network.edges = [8, 9, 10].map((id) => ({ id: `root:${id}`, kind: 'relationship' as const, source_node_id: 'person:7', target_node_id: `person:${id}`, label: 'knows' }));
    network.edges.push({ id: 'outer', kind: 'relationship', source_node_id: 'person:8', target_node_id: 'person:10', label: 'mentors', relationship_type_slug: 'mentor' });
    const controller = networkController(network);
    await controller.refreshRelationshipTypes();
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Peer 2' });
    const chip = container.querySelector<HTMLElement>('.edge-label[title="mentors"]')!;
    expect(container.querySelectorAll('path')[3]!.getAttribute('d')).toBe('M 280 14 H 272 V 126 H 280');
    expect(parseFloat(chip.style.left)).toBe(272);
    const y = parseFloat(chip.style.top);
    expect(y).toBeGreaterThan(94);
    expect(y).toBeLessThanOrEqual(103.6);
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
  });

  it('discards a stale depth response and shows the truncation limit', async () => {
    const depthTwo = deferredResponse();
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = requestOf(input);
      requests.push(request);
      const depth = Number(new URL(request.url).searchParams.get('depth'));
      if (depth === 2) return depthTwo.promise;
      return Response.json(depth === 3 ? { ...depthThree(), truncated: true } : connected(1));
    }));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByRole('button', { name: 'Curated Peer' });
    await fireEvent.click(screen.getByRole('radio', { name: '2 steps' }));
    await waitFor(() => expect(requests.some((request) => new URL(request.url).searchParams.get('depth') === '2')).toBe(true));
    await fireEvent.click(screen.getByRole('radio', { name: '3 steps' }));
    await screen.findByRole('button', { name: 'Depth Three Person' });
    depthTwo.resolve(Response.json(connected(2)));
    await waitFor(() => expect(screen.getByText('Showing up to 250 people and organizations and 500 connections.')).toBeDefined());
    expect(screen.getByRole('list', { name: 'Network connections' }).textContent).toContain('Depth Three Person');
  });
});
