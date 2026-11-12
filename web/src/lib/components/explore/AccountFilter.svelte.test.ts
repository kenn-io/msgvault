import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';
import AccountFilter from './AccountFilter.svelte';

function accountsResponse(virtualAccounts: unknown[]) {
  return vi.fn<typeof fetch>(async () =>
    Response.json({
      accounts: [{ id: 7, email: 'inbox@example.net', type: 'gmail', display_name: '', last_sync: null, message_count: 5, source_deleted_count: 0, virtual_accounts: virtualAccounts }],
    }),
  );
}

// One source with count messages awaiting account matching.
function pendingCatalog(count: number) {
  return Response.json({ accounts: [{ id: 7, email: 'inbox@example.net', virtual_accounts: [
    { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 0, source_deleted_count: 0, pending_count: count },
  ] }] });
}

describe('AccountFilter', () => {
  it('offers the unattributed bucket when only the inbox identity has mail', async () => {
    const fetchFn = accountsResponse([
      { key: 'identity:7:aW5ib3hAZXhhbXBsZS5uZXQ', source_id: 7, account_address: 'inbox@example.net', message_count: 3, source_deleted_count: 0 },
      { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 2, source_deleted_count: 0 },
    ]);
    const onChange = vi.fn();
    render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange });
    const select = await screen.findByRole('combobox', { name: /^Account:/ });
    await waitFor(() => expect(select).toHaveProperty('disabled', false));
    await chooseSelectOption(select, 'inbox@example.net / Unattributed (2)');
    expect(onChange).toHaveBeenCalledWith([{ dimension: 'account', values: ['unattributed:7'] }]);
  });

  it('reports pending repair and keeps other filters when selecting an alias under a padded source', async () => {
    const fetchFn = accountsResponse([
      { key: 'identity:7:d29ya0BleGFtcGxlLm9yZw', source_id: 7, account_address: 'work@example.org', message_count: 3, source_deleted_count: 0 },
      { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 1, source_deleted_count: 0, pending_count: 1 },
    ]);
    const onChange = vi.fn();
    const filters = [{ dimension: 'source' as const, values: [' +07'] }];
    render(AccountFilter, { client: createAPIClient(fetchFn), filters, onChange });
    expect((await screen.findByRole('status')).textContent).toContain('inbox@example.net: 1 message awaiting account matching.');
    const select = await screen.findByRole('combobox', { name: /^Account:/ });
    await waitFor(() => expect(select).toHaveProperty('disabled', false));
    await chooseSelectOption(select, 'inbox@example.net / work@example.org (3)');
    expect(onChange).toHaveBeenCalledWith([...filters, { dimension: 'account', values: ['identity:7:d29ya0BleGFtcGxlLm9yZw'] }]);
  });

  it.each([{ showSingleMailbox: false, count: 0 }, { showSingleMailbox: true, count: 1 }])('offers $count sole mailbox pickers when showSingleMailbox=$showSingleMailbox', async ({ showSingleMailbox, count }) => {
    const fetchFn = accountsResponse([
      { key: 'identity:7:aW5ib3hAZXhhbXBsZS5uZXQ', source_id: 7, account_address: 'inbox@example.net', message_count: 3, source_deleted_count: 0 },
    ]);
    const onChange = vi.fn();
    const filters = [{ dimension: 'account' as const, values: ['identity:7:aW5ib3hAZXhhbXBsZS5uZXQ'] }];
    const view = render(AccountFilter, { client: createAPIClient(fetchFn), filters, onChange, showSingleMailbox });
    const select = await screen.findByRole('combobox', { name: /^Account:/ });
    await waitFor(() => expect(select).toHaveProperty('disabled', false));
    await view.rerender({ filters: [] });
    expect(screen.queryAllByRole('combobox', { name: /^Account:/ })).toHaveLength(count);
  });

  it('retries an unavailable catalog until the receiving addresses arrive', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const account = { id: 7, email: 'inbox@example.net', type: 'gmail', display_name: '', last_sync: null, message_count: 5, source_deleted_count: 0 };
      const fetchFn = vi
        .fn<typeof fetch>()
        .mockImplementationOnce(async () =>
          Response.json({ accounts: [{ ...account, virtual_accounts: [] }], virtual_accounts_unavailable: true }),
        )
        .mockImplementation(async () =>
          Response.json({
            accounts: [
              {
                ...account,
                virtual_accounts: [
                  { key: 'identity:7:d29ya0BleGFtcGxlLm9yZw', source_id: 7, account_address: 'work@example.org', message_count: 3, source_deleted_count: 0 },
                  { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 2, source_deleted_count: 0 },
                ],
              },
            ],
          }),
        );
      const onChange = vi.fn();
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange });
      expect((await screen.findByRole('status')).textContent).toContain('Loading accounts');

      await vi.advanceTimersByTimeAsync(2000);
      const select = await screen.findByRole('combobox', { name: /^Account:/ });
      await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
      await chooseSelectOption(select, 'inbox@example.net / work@example.org (3)');
      expect(onChange).toHaveBeenCalledWith([{ dimension: 'account', values: ['identity:7:d29ya0BleGFtcGxlLm9yZw'] }]);
      expect(fetchFn).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps retrying when a reread fails while the catalog recovers', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const account = { id: 7, email: 'inbox@example.net', type: 'gmail', display_name: '', last_sync: null, message_count: 5, source_deleted_count: 0 };
      const fetchFn = vi
        .fn<typeof fetch>()
        .mockImplementationOnce(async () =>
          Response.json({ accounts: [{ ...account, virtual_accounts: [] }], virtual_accounts_unavailable: true }),
        )
        .mockImplementationOnce(async () => {
          throw new TypeError('network down');
        })
        .mockImplementation(async () =>
          Response.json({
            accounts: [
              {
                ...account,
                virtual_accounts: [
                  { key: 'identity:7:d29ya0BleGFtcGxlLm9yZw', source_id: 7, account_address: 'work@example.org', message_count: 3, source_deleted_count: 0 },
                ],
              },
            ],
          }),
        );
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      expect((await screen.findByRole('status')).textContent).toContain('Loading accounts');

      await vi.advanceTimersByTimeAsync(2000);
      await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(2));
      expect(screen.queryByRole('alert')).toBeNull();
      expect(screen.getByRole('status').textContent).toContain('Loading accounts');

      await vi.advanceTimersByTimeAsync(5000);
      await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(3));
      await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
      expect(screen.getByRole('combobox', { name: /^Account:/ })).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  it('offers immediate retry and keeps polling matching for the affected source', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let count = 2;
      let accept!: () => void;
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input as Request;
        if (request.method !== 'POST') return pendingCatalog(count);
        expect(request.url).toMatch(/\/sources\/7\/account-attribution\/repair$/);
        return new Promise<Response>((resolve) => {
          accept = () => {
            count = 1;
            resolve(new Response(null, { status: 202 }));
          };
        });
      });
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      await screen.findByText('inbox@example.net: 2 messages awaiting account matching.');
      const retry = await screen.findByRole('button', { name: 'Retry' });
      await fireEvent.click(retry);
      expect(retry).toHaveProperty('disabled', true);
      accept();
      await screen.findByText('inbox@example.net: 1 message awaiting account matching.');
      expect(screen.getByText('Retry requested')).toBeTruthy();
      expect(retry).toHaveProperty('disabled', false);
      expect(screen.queryByRole('alert')).toBeNull();
      count = 0;
      await vi.advanceTimersByTimeAsync(30000);
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull());
      expect(screen.queryByRole('status')).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps polling while matching remains pending', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const fetchFn = vi.fn<typeof fetch>(async () => pendingCatalog(2));
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      await screen.findByRole('button', { name: 'Retry' });
      await vi.advanceTimersByTimeAsync(300000);
      const calls = fetchFn.mock.calls.length;
      await vi.advanceTimersByTimeAsync(300000);
      expect(fetchFn.mock.calls.length).toBeGreaterThan(calls);
      expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops retrying persistent catalog errors after five minutes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const fetchFn = vi.fn<typeof fetch>(async () => { throw new Error('offline'); });
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      await screen.findByRole('alert');
      await vi.advanceTimersByTimeAsync(300000);
      const calls = fetchFn.mock.calls.length;
      await vi.advanceTimersByTimeAsync(300000);
      expect(fetchFn).toHaveBeenCalledTimes(calls);
    } finally {
      vi.useRealTimers();
    }
  });

  it('discovers pending matching after an empty catalog and stops polling on disposal', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let count = 0;
      const fetchFn = vi.fn<typeof fetch>(async () => pendingCatalog(count));
      const view = render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      await waitFor(() => expect(fetchFn).toHaveBeenCalledTimes(1));
      count = 2;
      await vi.advanceTimersByTimeAsync(30000);
      expect(screen.getByText('inbox@example.net: 2 messages awaiting account matching.')).toBeTruthy();
      expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
      view.unmount();
      const calls = fetchFn.mock.calls.length;
      await vi.advanceTimersByTimeAsync(60000);
      expect(fetchFn).toHaveBeenCalledTimes(calls);
    } finally {
      vi.useRealTimers();
    }
  });

  it('ignores a stale poll that returns after a retry', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let holdNext = false;
      let retried = false;
      let releaseBody: () => void = () => {};
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input as Request;
        if (request.method === 'POST') {
          retried = true;
          return new Response(null, { status: 202 });
        }
        if (holdNext) {
          holdNext = false;
          const response = pendingCatalog(2);
          const body = await response.text();
          // Parsing can resume after the request is canceled once the body is buffered.
          response.text = () => new Promise<string>((resolve) => { releaseBody = () => resolve(body); });
          return response;
        }
        return pendingCatalog(retried ? 0 : 2);
      });
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      await screen.findByText('inbox@example.net: 2 messages awaiting account matching.');
      expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
      holdNext = true;
      await vi.advanceTimersByTimeAsync(2000);
      expect(holdNext).toBe(false);
      await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull());
      releaseBody();
      await vi.advanceTimersByTimeAsync(1);
      expect(screen.queryByText('inbox@example.net: 2 messages awaiting account matching.')).toBeNull();
      expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it('retains pending sources after a failed retry and an unavailable catalog', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let unavailable = false;
      const fetchFn = vi.fn<typeof fetch>(async (input) => {
        const request = input as Request;
        if (request.method === 'POST') return Response.json({ error: 'failed' }, { status: 500 });
        if (unavailable) return Response.json({ accounts: [{ id: 7, email: 'inbox@example.net', virtual_accounts: [] }], virtual_accounts_unavailable: true });
        return pendingCatalog(2);
      });
      render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
      await screen.findByText('inbox@example.net: 2 messages awaiting account matching.');
      unavailable = true;
      await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
      expect((await screen.findByRole('alert')).textContent).toContain('Unable to retry account matching.');
      expect(screen.getByText('inbox@example.net: 2 messages awaiting account matching.')).toBeTruthy();
      const calls = fetchFn.mock.calls.length;
      await vi.advanceTimersByTimeAsync(30000);
      expect(fetchFn.mock.calls.length).toBeGreaterThan(calls);
      expect(screen.getByRole('button', { name: 'Retry' })).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });

  it('labels a saved pick of a source that lists no entries', async () => {
    const inboxKey = 'identity:7:aW5ib3hAZXhhbXBsZS5uZXQ';
    const fetchFn = accountsResponse([
      { key: inboxKey, source_id: 7, account_address: 'inbox@example.net', message_count: 3, source_deleted_count: 0 },
    ]);
    render(AccountFilter, {
      client: createAPIClient(fetchFn),
      filters: [{ dimension: 'account', values: [inboxKey] }],
      onChange: vi.fn(),
    });
    const select = await screen.findByRole('combobox', { name: /^Account:/ });
    await waitFor(() => expect(select).toHaveProperty('disabled', false));
    expect(select.textContent).toContain('inbox@example.net / inbox@example.net (3)');
    expect(select.textContent).not.toContain('unavailable');
  });

  it('drops a pick from a source the source filter excludes', async () => {
    const fetchFn = accountsResponse([
      { key: 'identity:7:d29ya0BleGFtcGxlLm9yZw', source_id: 7, account_address: 'work@example.org', message_count: 3, source_deleted_count: 0 },
      { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 2, source_deleted_count: 0 },
    ]);
    const onChange = vi.fn();
    const source = { dimension: 'source' as const, values: ['9'] };
    render(AccountFilter, {
      client: createAPIClient(fetchFn),
      filters: [source, { dimension: 'account', values: ['unattributed:7'] }],
      onChange,
    });
    await waitFor(() => expect(onChange).toHaveBeenCalledWith([source]));
  });
});
