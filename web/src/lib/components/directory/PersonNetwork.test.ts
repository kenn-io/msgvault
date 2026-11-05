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

describe('PersonNetwork', () => {
  it('centers relation chips on cross-column and same-column lines and opens records', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(connected())));
    const controller = new DirectoryEntityController(client, 7);
    const onOpenPerson = vi.fn();
    const onOpenOrganization = vi.fn();
    const { container } = render(PersonNetwork, { controller, onOpenPerson, onOpenOrganization });

    const list = await screen.findByRole('list', { name: 'Network connections' });
    expect([...list.querySelectorAll('li')].map((item) => item.textContent)).toEqual([
      'Selected Person · works with · Curated Peer',
      'Curated Peer · Engineer · Shared Organization',
      'Curated Peer · mentors · Second Hop Person'
    ]);
    const lines = container.querySelectorAll('line');
    const chips = container.querySelectorAll<HTMLElement>('.edge-label');
    expect(chips).toHaveLength(3);
    for (const index of [0, 1]) {
      const line = lines[index]!;
      expect(parseFloat(chips[index]!.style.left)).toBe((Number(line.getAttribute('x1')) + Number(line.getAttribute('x2'))) / 2);
      expect(parseFloat(chips[index]!.style.top)).toBe((Number(line.getAttribute('y1')) + Number(line.getAttribute('y2'))) / 2);
    }
    expect(lines[0]!.getAttribute('x1')).not.toBe(lines[0]!.getAttribute('x2'));
    expect(lines[1]!.getAttribute('x1')).toBe(lines[1]!.getAttribute('x2'));
    expect(Math.abs(Number(lines[1]!.getAttribute('y1')) - Number(lines[1]!.getAttribute('y2')))).toBe(112);
    await fireEvent.click(screen.getByRole('button', { name: 'Curated Peer' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Shared Organization' }));
    expect(onOpenPerson).toHaveBeenCalledWith(8);
    expect(onOpenOrganization).toHaveBeenCalledWith(21);
    expect(screen.queryByRole('button', { name: 'Selected Person' })).toBeNull();
    expect(screen.getByRole('region', { name: 'Network graph' }).tabIndex).toBe(0);
  });

  it('shares one chip for two relations between the same pair and retains both semantic edges', async () => {
    const network = connected();
    network.edges.push({ ...network.edges[0]!, id: 'relationship:34', source_node_id: 'person:8', target_node_id: 'person:7', label: 'mentored by' });
    const controller = new DirectoryEntityController(createAPIClient(vi.fn<typeof fetch>(async () => Response.json(network))), 7);
    const { container } = render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    await screen.findByText('works with · mentored by');
    expect(container.querySelectorAll('.edge-label')).toHaveLength(3);
    expect(screen.getByRole('list', { name: 'Network connections' }).querySelectorAll('li')).toHaveLength(4);
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
