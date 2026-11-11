import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import FilterMenu from './FilterMenu.svelte';

it("offers the server's Conversation kind for an empty message type in a group chat", async () => {
  const requests: Request[] = [];
  const onChange = vi.fn();
  render(FilterMenu, {
    client: createAPIClient(async (input) => {
      const request = input as Request;
      if (!request.url.endsWith('/groups')) return Response.json({ accounts: [], identities: [] });
      requests.push(request);
      const body = await request.clone().json();
      // An empty message_type in group_chat is canonically a conversation,
      // while grouping by message_type folds it into the email bucket.
      return Response.json({ rows: body.grouping?.[0] === 'kind'
        ? [{ key: 'conversation', count: 1 }]
        : [{ key: 'email', count: 1 }] });
    }), filters: [], onChange, onClear: vi.fn(),
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByRole('checkbox', { name: 'Conversation' }));
  expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'kind', values: ['conversation'] }]);
  await expect(requests[0]!.clone().json()).resolves.toMatchObject({ grouping: ['kind'] });
});

it('edits UTC date bounds and retains committed bounds after invalid edits', async () => {
  const onChange = vi.fn();
  const filters = [{ dimension: 'after' as const, values: ['2026-01-01T00:00:00.000Z'] }, { dimension: 'before' as const, values: ['2027-01-01T00:00:00.000Z'] }];
  const props = { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), onChange, onClear: vi.fn() };
  const view = render(FilterMenu, { ...props, filters });
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
  await view.rerender({ ...props, filters: [] });
  await fireEvent.change(from, { target: { value: '2026-07-01' } });
  expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'after', values: ['2026-07-01T00:00:00.000Z'] }]);
});

it('keeps Kind lookup failures visible and retries when Filter reopens', async () => {
  let failed = true;
  render(FilterMenu, {
    client: createAPIClient(async (input) => {
      if (new URL((input as Request).url).pathname.endsWith('/groups')) {
        return failed ? Response.json({ message: 'Unavailable' }, { status: 500 }) : Response.json({ rows: [{ key: 'email', count: 1 }] });
      }
      return Response.json({ accounts: [], identities: [] });
    }), filters: [], onChange: vi.fn(), onClear: vi.fn(),
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(await screen.findByText('Could not load kinds')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  failed = false;
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  expect(await screen.findByLabelText('Email')).toBeDefined();
  expect(screen.queryByText('Could not load kinds')).toBeNull();
});

it.each([
  { name: 'preserves restored kinds when adding Email', filters: [{ dimension: 'kind' as const, values: ['conversation'] }], kinds: ['conversation', 'email'], label: 'Email', expected: ['conversation', 'email'] },
  { name: 'selects the canonical meeting kind in a meetings-only archive', filters: [], kinds: ['meeting'], label: 'Meeting', expected: ['meeting'] },
  { name: 'selects conversations across message types', filters: [], kinds: ['conversation'], label: 'Conversation', expected: ['conversation'] },
])('$name', async ({ filters, kinds, label, expected }) => {
  const onChange = vi.fn();
  render(FilterMenu, {
    client: createAPIClient(async (input) => new URL((input as Request).url).pathname.endsWith('/groups')
      ? Response.json({ rows: kinds.map(key => ({ key, count: 1 })) })
      : Response.json({ accounts: [], identities: [] })),
    filters, onChange, onClear: vi.fn(),
  });
  await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
  await fireEvent.click(await screen.findByRole('checkbox', { name: label }));
  expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'kind', values: expected }]);
});

it('Escape closes only the active Account picker, then Filter', async () => {
  render(FilterMenu, { client: createAPIClient(async (input) => {
    const url = (input as Request).url;
    return Response.json(url.endsWith('/accounts') ? { accounts: [{ id: 1, email: 'first@example.com', virtual_accounts: [
      { key: 'identity:1:alias', source_id: 1, account_address: 'alias@example.com', message_count: 3, source_deleted_count: 0 },
    ] }] } : url.includes('/identities') ? { identities: [{ identifier: 'alias@example.com' }] } : { rows: [] });
  }), filters: [{ dimension: 'source', values: ['1'] }], onChange: vi.fn(), onClear: vi.fn() });
  const trigger = screen.getByRole('button', { name: 'Filter' });
  await fireEvent.click(trigger);
  const select = await screen.findByRole('combobox', { name: /^Account:/ });
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

it('displays local dates with UTC peers', async () => {
  vi.stubEnv('TZ', 'Europe/London');
  try {
    render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), onChange: vi.fn(), onClear: vi.fn(), filters: [
      { dimension: 'after', values: ['2026-07-01T00:00:00-04:00'] }, { dimension: 'before', values: ['2026-08-01T00:00:00Z'] }
    ] });
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    expect((screen.getByLabelText('From') as HTMLInputElement).value).toBe('2026-07-01');
    expect((screen.getByLabelText('To (UTC)') as HTMLInputElement).value).toBe('2026-07-31');
  } finally { vi.unstubAllEnvs(); }
});

it.each([
  { name: 'ends a local date across daylight saving', zone: 'America/New_York', filters: [{ dimension: 'before', values: ['2024-04-01T00:00:00-04:00'] }], field: 'To', date: '2024-03-10', expected: [{ dimension: 'before', values: ['2024-03-11T00:00:00-04:00'] }] },
  { name: 'edits a zero-offset local bound in summer', zone: 'Europe/London', filters: [{ dimension: 'after', values: ['2026-01-01T00:00:00+00:00'] }], field: 'From', date: '2026-07-01', expected: [{ dimension: 'after', values: ['2026-07-01T00:00:00+01:00'] }] },
  { name: 'uses the peer calendar for a new bound', zone: 'Europe/London', filters: [{ dimension: 'after', values: ['2026-01-01T00:00:00+00:00'] }], field: 'To', date: '2026-07-01', expected: [{ dimension: 'after', values: ['2026-01-01T00:00:00+00:00'] }, { dimension: 'before', values: ['2026-07-02T00:00:00+01:00'] }] }
] as const)('$name', async ({ zone, filters, field, date, expected }) => {
  vi.stubEnv('TZ', zone);
  try {
    const onChange = vi.fn();
    render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), onChange, onClear: vi.fn(), filters: filters.map(({ dimension, values }) => ({ dimension, values: [...values] })) });
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    await fireEvent.change(screen.getByLabelText(field), { target: { value: date } });
    expect(onChange).toHaveBeenLastCalledWith(expected);
  } finally { vi.unstubAllEnvs(); }
});
