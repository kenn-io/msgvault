import { render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';
import AccountFilter from './AccountFilter.svelte';

describe('AccountFilter', () => {
  it('reports pending repair when the only identity is the source inbox', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ accounts: [{
      id: 7, email: 'inbox@example.net', type: 'gmail', virtual_accounts: [
        { key: 'identity:7:aW5ib3hAZXhhbXBsZS5uZXQ', source_id: 7, account_address: 'inbox@example.net', message_count: 0 },
        { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 3, pending_count: 3 }
      ]
    }] }));
    render(AccountFilter, { client: createAPIClient(fetchFn), filters: [], onChange: vi.fn() });

    expect((await screen.findByRole('status')).textContent).toContain('Attribution repair pending for 3 messages.');
  });

  it('selects a collapsed masked group and preserves other filters', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ accounts: [{
      id: 7, email: 'inbox@example.net', type: 'gmail', virtual_accounts: [
        { key: 'identity:7:d29yaw', source_id: 7, account_address: 'work@example.org', message_count: 3 },
        { key: 'group:7:bWFza3M', source_id: 7, group: 'fastmail-masked:inbox@example.net', message_count: 1500 },
        { key: 'unattributed:7', source_id: 7, unattributed: true, message_count: 2, pending_count: 1 }
      ]
    }] }));
    const onChange = vi.fn();
    const filters = [{ dimension: 'source' as const, values: ['7'] }, { dimension: 'domain' as const, values: ['example.org'] }];
    render(AccountFilter, { client: createAPIClient(fetchFn), filters, onChange });
    const select = await screen.findByRole('combobox', { name: /^Account:/ });
    await waitFor(() => expect(select).toHaveProperty('disabled', false));
    await chooseSelectOption(select, 'inbox@example.net / fastmail-masked:inbox@example.net (1500)');
    expect(onChange).toHaveBeenCalledWith([...filters, { dimension: 'account', values: ['group:7:bWFza3M'] }]);
  });
});
