import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import FilterMenu from './FilterMenu.svelte';

it('offers ordinary confirmed mailboxes with their catalog keys', async () => {
  const onChange = vi.fn();
  render(FilterMenu, { client: createAPIClient(async (input) => Response.json((input as Request).url.endsWith('/accounts') ? { accounts: [
    { id: 1, email: 'first@example.com', virtual_accounts: [{ key: 'identity:1:first', source_id: 1, account_address: 'first@example.com', message_count: 3, source_deleted_count: 0 }] },
    { id: 2, email: 'second@example.com', virtual_accounts: [{ key: 'identity:2:second', source_id: 2, account_address: 'second@example.com', message_count: 4, source_deleted_count: 0 }] },
  ] } : { rows: [] })), filters: [], onChange });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByRole('combobox', { name: /^Account:/ }));
  expect(screen.getByRole('option', { name: 'first@example.com / first@example.com (3)' })).toBeDefined();
  await fireEvent.click(screen.getByRole('option', { name: 'second@example.com / second@example.com (4)' }));
  expect(onChange).toHaveBeenCalledWith([{ dimension: 'account', values: ['identity:2:second'] }]);
});

it.each(['Account', 'Identity'])('Escape closes only the active %s picker, then Filter', async (picker) => {
  render(FilterMenu, { client: createAPIClient(async (input) => {
    const url = (input as Request).url;
    return Response.json(url.endsWith('/accounts') ? { accounts: [{ id: 1, email: 'first@example.com', virtual_accounts: [
      { key: 'identity:1:alias', source_id: 1, account_address: 'alias@example.com', message_count: 3, source_deleted_count: 0 },
    ] }] } : url.includes('/identities') ? { identities: [{ identifier: 'alias@example.com' }] } : { rows: [] });
  }), filters: [{ dimension: 'source', values: ['1'] }], onChange: vi.fn() });
  const trigger = screen.getByRole('button', { name: 'Filter' });
  await fireEvent.click(trigger);
  const select = await screen.findByRole('combobox', { name: new RegExp(`^${picker}:`) });
  await waitFor(() => expect(select).toHaveProperty('disabled', false));
  await fireEvent.click(select);
  await screen.findByRole('listbox');
  await fireEvent.keyDown(select, { key: 'Escape' });
  expect(screen.getByRole('dialog', { name: 'Filter' })).toBeDefined();
  expect(screen.queryByRole('listbox')).toBeNull();
  expect(document.activeElement).toBe(select);
  await fireEvent.keyDown(select, { key: 'Escape' });
  expect(screen.queryByRole('dialog', { name: 'Filter' })).toBeNull();
  expect(document.activeElement).toBe(trigger);
});

it('shows UTC year bounds and edits only the changed boundary', async () => {
  const onChange = vi.fn();
  const filters = [{ dimension: 'after' as const, values: ['2026-01-01T00:00:00.000Z'] }, { dimension: 'before' as const, values: ['2027-01-01T00:00:00.000Z'] }];
  render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters, onChange });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect((screen.getByLabelText('From') as HTMLInputElement).value).toBe('2026-01-01');
  expect((screen.getByLabelText('To') as HTMLInputElement).value).toBe('2026-12-31');
  await fireEvent.change(screen.getByLabelText('To'), { target: { value: '2026-11-30' } });
  expect(onChange).toHaveBeenLastCalledWith([filters[0], { dimension: 'before', values: ['2026-12-01T00:00:00.000Z'] }]);
  await fireEvent.change(screen.getByLabelText('From'), { target: { value: '2026-02-01' } });
  expect(onChange).toHaveBeenLastCalledWith([filters[1], { dimension: 'after', values: ['2026-02-01T00:00:00.000Z'] }]);
});
