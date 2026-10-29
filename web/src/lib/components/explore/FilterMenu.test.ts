import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import FilterMenu from './FilterMenu.svelte';

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
  const from = screen.getByLabelText('From') as HTMLInputElement;
  const to = screen.getByLabelText('To') as HTMLInputElement;
  onChange.mockClear();
  await fireEvent.change(from, { target: { value: '2027-01-01' } });
  expect(screen.getByRole('alert').textContent).toBe('From must be on or before To.');
  expect(from.value).toBe('2026-01-01');
  await fireEvent.change(to, { target: { value: '2025-12-31' } });
  expect(to.value).toBe('2026-12-31');
  expect(onChange).not.toHaveBeenCalled();
  await fireEvent.change(from, { target: { value: '2026-12-31' } });
  expect(onChange).toHaveBeenLastCalledWith([filters[1], { dimension: 'after', values: ['2026-12-31T00:00:00.000Z'] }]);
  expect(screen.queryByRole('alert')).toBeNull();
  await fireEvent.change(from, { target: { value: '' } });
  expect(onChange).toHaveBeenLastCalledWith([filters[1]]);
});

it('keeps Kind lookup failures visible and retries when Filter reopens', async () => {
  let failed = true;
  render(FilterMenu, {
    client: createAPIClient(async (input) => {
      if (new URL((input as Request).url).pathname.endsWith('/groups')) {
        return failed ? Response.json({ message: 'Unavailable' }, { status: 500 }) : Response.json({ rows: [{ key: 'email', count: 1 }] });
      }
      return Response.json({ accounts: [], identities: [] });
    }), filters: [], onChange: vi.fn(),
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(await screen.findByText('Could not load kinds')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(await screen.findByLabelText('Email')).toBeDefined();
  expect(screen.queryByText('Could not load kinds')).toBeNull();
});

it('keeps the selected chat network and restored kinds when Email is added', async () => {
  const onChange = vi.fn();
  const view = render(FilterMenu, {
    client: createAPIClient(async (input) => {
      if (new URL((input as Request).url).pathname.endsWith('/groups')) return Response.json({ rows: [{ key: 'whatsapp' }, { key: 'discord' }, { key: 'email' }] });
      return Response.json({ accounts: [], identities: [] });
    }), filters: [{ dimension: 'message_type', values: ['whatsapp', 'restored-kind'] }], onChange,
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByLabelText('Email'));
  expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'message_type', values: ['whatsapp', 'restored-kind', 'email'] }]);
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await view.rerender({ filters: [] });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByLabelText('Email'));
  expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'message_type', values: ['email'] }]);
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
