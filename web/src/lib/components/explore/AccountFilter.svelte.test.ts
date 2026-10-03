import { render, screen, waitFor } from '@testing-library/svelte';
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

  it('reports pending repair and keeps other filters when selecting an alias', async () => {
    const fetchFn = accountsResponse([
      { key: 'identity:7:d29ya0BleGFtcGxlLm9yZw', source_id: 7, account_address: 'work@example.org', message_count: 3, source_deleted_count: 0 },
      { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 1, source_deleted_count: 0, pending_count: 1 },
    ]);
    const onChange = vi.fn();
    const filters = [{ dimension: 'source' as const, values: ['7'] }];
    render(AccountFilter, { client: createAPIClient(fetchFn), filters, onChange });
    expect((await screen.findByRole('status')).textContent).toContain('Attribution repair pending for 1 messages.');
    const select = await screen.findByRole('combobox', { name: /^Account:/ });
    await waitFor(() => expect(select).toHaveProperty('disabled', false));
    await chooseSelectOption(select, 'inbox@example.net / work@example.org (3)');
    expect(onChange).toHaveBeenCalledWith([...filters, { dimension: 'account', values: ['identity:7:d29ya0BleGFtcGxlLm9yZw'] }]);
  });

  it('stays hidden when the inbox is the only account and nothing is unattributed', async () => {
    const fetchFn = accountsResponse([
      { key: 'identity:7:aW5ib3hAZXhhbXBsZS5uZXQ', source_id: 7, account_address: 'inbox@example.net', message_count: 3, source_deleted_count: 0 },
    ]);
    render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });
    await waitFor(() => expect(fetchFn).toHaveBeenCalled());
    expect(screen.queryByRole('combobox', { name: /^Account:/ })).toBeNull();
  });
});
