import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { ExploreFilter } from '../../explore/models';
import IdentityFilter from './IdentityFilter.svelte';

function catalog(identities: string[] = ['primary@example.test', 'shop@example.test']) {
  return {
    source_id: 14,
    account: 'Fastmail — primary@example.test',
    identities: identities.map((identifier) => ({
      identifier,
      signals: ['provider'],
      confirmed_at: '2026-08-03T00:00:00Z'
    }))
  };
}

describe('IdentityFilter', () => {
  it('appears only for exactly one source and does not create source or account options', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json(catalog()));
    const rendered = render(IdentityFilter, {
      client: createAPIClient(fetchFn),
      filters: [],
      onChange: vi.fn()
    });

    expect(screen.queryByRole('combobox')).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();

    await rendered.rerender({
      client: createAPIClient(fetchFn),
      filters: [{ dimension: 'source', values: ['14', '15'] }],
      onChange: vi.fn()
    });
    expect(screen.queryByRole('combobox')).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();

    await rendered.rerender({
      client: createAPIClient(fetchFn),
      filters: [{ dimension: 'source', values: ['14'] }],
      onChange: vi.fn()
    });

    const identity = await screen.findByRole('combobox', { name: 'Identity' });
    expect(fetchFn).toHaveBeenCalledOnce();
    expect(new URL((fetchFn.mock.calls[0]![0] as Request).url).pathname)
      .toBe('/api/v1/sources/14/identities');
    await waitFor(() => expect(within(identity).getAllByRole('option')).toHaveLength(3));
    expect(within(identity).queryByRole('option', { name: 'Fastmail — primary@example.test' })).toBeNull();
    expect(within(identity).getAllByRole('option').map((option) => option.textContent)).toEqual([
      'Any confirmed identity',
      'primary@example.test',
      'shop@example.test'
    ]);
    expect(screen.getAllByRole('combobox')).toHaveLength(2);
  });

  it('emits a complete replacement list while preserving its parent source and unrelated filters', async () => {
    const onChange = vi.fn<(filters: ExploreFilter[]) => void>();
    const filters: ExploreFilter[] = [
      { dimension: 'source', values: ['14'] },
      { dimension: 'message_type', values: ['email'] }
    ];
    render(IdentityFilter, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(catalog()))),
      filters,
      onChange
    });

    const identity = await screen.findByRole('combobox', { name: 'Identity' });
    await waitFor(() => expect(within(identity).getAllByRole('option')).toHaveLength(3));
    await fireEvent.change(identity, {
      target: { value: 'shop@example.test' }
    });

    expect(onChange).toHaveBeenLastCalledWith([
      { dimension: 'source', values: ['14'] },
      { dimension: 'message_type', values: ['email'] },
      { dimension: 'identity', values: ['14', 'shop@example.test', 'any'] }
    ]);
  });

  it('restores the direction and exposes the required keyboard-operable labels', async () => {
    const onChange = vi.fn<(filters: ExploreFilter[]) => void>();
    const filters: ExploreFilter[] = [
      { dimension: 'source', values: ['14'] },
      { dimension: 'identity', values: ['14', 'shop@example.test', 'recipient'] }
    ];
    render(IdentityFilter, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(catalog()))),
      filters,
      onChange
    });

    const identity = await screen.findByRole('combobox', { name: 'Identity' });
    await waitFor(() => expect(within(identity).getAllByRole('option')).toHaveLength(3));
    expect((identity as HTMLSelectElement).value).toBe('shop@example.test');
    const direction = screen.getByRole('combobox', { name: 'Identity direction' });
    expect(within(direction).getAllByRole('option').map((option) => option.textContent))
      .toEqual(['Any', 'Sent via', 'Received via']);
    expect((direction as HTMLSelectElement).value).toBe('recipient');

    await fireEvent.change(direction, { target: { value: 'sender' } });
    expect(onChange).toHaveBeenLastCalledWith([
      { dimension: 'source', values: ['14'] },
      { dimension: 'identity', values: ['14', 'shop@example.test', 'sender'] }
    ]);
  });

  it('clears identity when its parent source changes or is removed', async () => {
    const onChange = vi.fn<(filters: ExploreFilter[]) => void>();
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json(catalog())));
    const rendered = render(IdentityFilter, {
      client,
      filters: [
        { dimension: 'source', values: ['14'] },
        { dimension: 'identity', values: ['14', 'shop@example.test', 'recipient'] }
      ],
      onChange
    });
    await screen.findByRole('combobox', { name: 'Identity' });

    await rendered.rerender({
      client,
      filters: [
        { dimension: 'source', values: ['15'] },
        { dimension: 'identity', values: ['14', 'shop@example.test', 'recipient'] }
      ],
      onChange
    });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith([
      { dimension: 'source', values: ['15'] }
    ]));

    onChange.mockClear();
    await rendered.rerender({
      client,
      filters: [
        { dimension: 'source', values: ['15'] },
        { dimension: 'identity', values: ['15', 'shop@example.test', 'any'] }
      ],
      onChange
    });
    await rendered.rerender({
      client,
      filters: [{ dimension: 'identity', values: ['15', 'shop@example.test', 'any'] }],
      onChange
    });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith([]));
  });

  it('names loading, empty, and error catalog states', async () => {
    let resolveCatalog!: (response: Response) => void;
    const loadingFetch = vi.fn<typeof fetch>(() => new Promise((resolve) => { resolveCatalog = resolve; }));
    const loading = render(IdentityFilter, {
      client: createAPIClient(loadingFetch),
      filters: [{ dimension: 'source', values: ['14'] }],
      onChange: vi.fn()
    });
    expect((await screen.findByRole('status')).textContent).toContain('Loading identities…');
    resolveCatalog(Response.json(catalog([])));
    await waitFor(() => expect(screen.getByRole('status').textContent)
      .toContain('No confirmed identities for this source.'));
    loading.unmount();

    render(IdentityFilter, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(
        { error: 'catalog_unavailable', message: 'Synthetic catalog failure.' },
        { status: 500 }
      ))),
      filters: [{ dimension: 'source', values: ['14'] }],
      onChange: vi.fn()
    });
    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load confirmed identities.');
  });

  it('ignores a stale catalog response after the parent source changes', async () => {
    let resolveFirst!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (new URL(request.url).pathname.endsWith('/14/identities')) {
        return await new Promise<Response>((resolve) => { resolveFirst = resolve; });
      }
      return Response.json({
        ...catalog(['new-source@example.test']),
        source_id: 15
      });
    });
    const client = createAPIClient(fetchFn);
    const rendered = render(IdentityFilter, {
      client,
      filters: [{ dimension: 'source', values: ['14'] }],
      onChange: vi.fn()
    });
    await waitFor(() => expect(fetchFn).toHaveBeenCalledOnce());

    await rendered.rerender({
      client,
      filters: [{ dimension: 'source', values: ['15'] }],
      onChange: vi.fn()
    });
    expect(await screen.findByRole('option', { name: 'new-source@example.test' })).toBeDefined();

    resolveFirst(Response.json(catalog(['stale-source@example.test'])));
    await Promise.resolve();
    expect(screen.queryByRole('option', { name: 'stale-source@example.test' })).toBeNull();
    expect(screen.getByRole('option', { name: 'new-source@example.test' })).toBeDefined();
  });

  it('keeps an active identity filter removable when the catalog request fails', async () => {
    const onChange = vi.fn<(filters: ExploreFilter[]) => void>();
    const filters: ExploreFilter[] = [
      { dimension: 'source', values: ['14'] },
      { dimension: 'identity', values: ['14', 'shop@example.test', 'recipient'] }
    ];
    render(IdentityFilter, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => {
        throw new Error('Synthetic network failure.');
      })),
      filters,
      onChange
    });

    const identity = await screen.findByRole('combobox', { name: 'Identity' });
    await screen.findByRole('alert');
    expect((identity as HTMLSelectElement).disabled).toBe(false);

    await fireEvent.change(identity, { target: { value: '' } });
    expect(onChange).toHaveBeenLastCalledWith([
      { dimension: 'source', values: ['14'] }
    ]);
  });

  it('renders a restored identity missing from the catalog as an unavailable option', async () => {
    const onChange = vi.fn<(filters: ExploreFilter[]) => void>();
    const filters: ExploreFilter[] = [
      { dimension: 'source', values: ['14'] },
      { dimension: 'identity', values: ['14', 'gone@example.test', 'any'] }
    ];
    render(IdentityFilter, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(catalog()))),
      filters,
      onChange
    });

    const identity = await screen.findByRole('combobox', { name: 'Identity' });
    await waitFor(() => expect(within(identity).getAllByRole('option')).toHaveLength(4));
    const unavailable = within(identity).getByRole('option', { name: 'gone@example.test (unavailable)' });
    expect((unavailable as HTMLOptionElement).value).toBe('gone@example.test');
    expect((identity as HTMLSelectElement).value).toBe('gone@example.test');

    await fireEvent.change(identity, { target: { value: '' } });
    expect(onChange).toHaveBeenLastCalledWith([
      { dimension: 'source', values: ['14'] }
    ]);
  });

  it('does not flash a synthetic option while the catalog is loading', async () => {
    const filters: ExploreFilter[] = [
      { dimension: 'source', values: ['14'] },
      { dimension: 'identity', values: ['14', 'gone@example.test', 'any'] }
    ];
    render(IdentityFilter, {
      client: createAPIClient(vi.fn<typeof fetch>(() => new Promise(() => {}))),
      filters,
      onChange: vi.fn()
    });

    const identity = await screen.findByRole('combobox', { name: 'Identity' });
    await screen.findByRole('status');
    expect(within(identity).queryByRole('option', { name: 'gone@example.test (unavailable)' })).toBeNull();
  });
});
