import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { DirectoryEntityController } from '../../directory/entity-controller.svelte';
import type { PersonNetwork as PersonNetworkProjection } from '../../directory/models';
import { chooseSelectOption } from '../../../test/kit-ui';
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
  it('sizes the SVG for a hop-three node, its circle, and a bounded label lane', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const depth = Number(new URL(requestOf(input).url).searchParams.get('depth'));
      return Response.json(depth === 3 ? depthThree() : rootOnly(depth));
    }));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });
    const initialSVG = await waitFor(() => {
      const svg = document.querySelector<SVGSVGElement>('.projection > svg');
      expect(svg).not.toBeNull();
      return svg!;
    });
    expect(initialSVG.viewBox.baseVal.width).toBe(650);

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Network depth:/ }), '3 hops');
    await screen.findByRole('button', { name: 'Open person Depth Three Person' });
    const svg = document.querySelector<SVGSVGElement>('.projection > svg')!;
    const group = [...svg.querySelectorAll('g')].find((item) => item.textContent?.includes('Depth Three Person'))!;
    const originX = Number(group.getAttribute('transform')?.match(/translate\((\d+)/)?.[1]);
    const radius = Number(group.querySelector('circle')?.getAttribute('r'));
    const labelX = Number(group.querySelector('text')?.getAttribute('x'));
    const width = svg.viewBox.baseVal.width;

    expect(width).toBeGreaterThanOrEqual(originX + radius);
    expect(width).toBeGreaterThanOrEqual(originX + labelX + 160);
    expect(Number(svg.getAttribute('width'))).toBe(width);
  });

  it('keeps a root-only projection in the semantic list while hiding the progressive SVG', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(rootOnly())));
    const controller = new DirectoryEntityController(client, 7);

    render(PersonNetwork, { controller, onOpenPerson: vi.fn(), onOpenOrganization: vi.fn() });

    const list = await screen.findByRole('list', { name: 'Directory network connections' });
    await waitFor(() => expect(list.textContent).toContain('Selected Person'));
    expect(screen.getByText('No curated connections at this depth.')).toBeDefined();
    expect(document.querySelector('.projection > svg')?.getAttribute('aria-hidden')).toBe('true');
  });

  it('groups every typed relationship and employment by hop and makes both entity kinds actionable', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(connected())));
    const controller = new DirectoryEntityController(client, 7);
    const onOpenPerson = vi.fn();
    const onOpenOrganization = vi.fn();

    render(PersonNetwork, { controller, onOpenPerson, onOpenOrganization });

    const list = await screen.findByRole('list', { name: 'Directory network connections' });
    await waitFor(() => expect(list.textContent).toContain('Hop 1'));
    const listText = list.textContent?.replace(/\s+/g, ' ') ?? '';
    expect(listText).toContain('Hop 2');
    expect(listText).toContain('Selected Person works with Curated Peer');
    expect(listText).toContain('Curated Peer Engineer Shared Organization');
    expect(listText).toContain('Curated Peer mentors Second Hop Person');

    await fireEvent.click(screen.getAllByRole('button', { name: 'Open person Curated Peer' })[0]!);
    await fireEvent.click(screen.getByRole('button', { name: 'Open organization Shared Organization' }));
    expect(onOpenPerson).toHaveBeenCalledWith(8);
    expect(onOpenOrganization).toHaveBeenCalledWith(21);
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

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Network depth:/ }), '2 hops');
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

    await chooseSelectOption(screen.getByRole('combobox', { name: /^Network depth:/ }), '3 hops');
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
    await screen.findByText('No curated connections at this depth.');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Network depth:/ }), '2 hops');
    await waitFor(() => expect(requests.some((request) => new URL(request.url).searchParams.get('depth') === '2')).toBe(true));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^Network depth:/ }), '3 hops');
    expect((await screen.findAllByText('Curated Peer')).length).toBeGreaterThan(0);
    depthTwo.resolve(Response.json(connected(2)));
    await waitFor(() => expect(screen.getByText(/bounded prefix/).textContent).toContain('at most 250 nodes and 500 connections at depth 3'));
    expect(screen.getByRole('list', { name: 'Directory network connections' }).textContent).toContain('Second Hop Person');
  });
});
