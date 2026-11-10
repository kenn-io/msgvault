import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import FilterMenu from './FilterMenu.svelte';

it('keeps a new London From date local when edited from January to July', async () => {
  vi.stubEnv('TZ', 'Europe/London');
  try {
    const onChange = vi.fn();
    const props = { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters: [], dateMode: 'auto' as const, onChange, onClear: vi.fn() };
    const view = render(FilterMenu, props);
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    await fireEvent.change(screen.getByLabelText('From'), { target: { value: '2026-01-01' } });
    const filters = onChange.mock.lastCall![0];
    expect(filters).toEqual([expect.objectContaining({ dimension: 'after', values: ['2026-01-01T00:00:00.000Z'] })]);
    await view.rerender({ ...props, filters });
    await fireEvent.change(screen.getByLabelText('From'), { target: { value: '2026-07-01' } });
    expect(onChange).toHaveBeenLastCalledWith([expect.objectContaining({ dimension: 'after', values: ['2026-06-30T23:00:00.000Z'] })]);
  } finally { vi.unstubAllEnvs(); }
});

it('keeps an existing local bound in Files while new Files bounds use UTC', async () => {
  vi.stubEnv('TZ', 'Europe/London');
  try {
    const filters = [{ dimension: 'after' as const, values: ['2026-01-01T00:00:00.000Z'], calendar: 'local' as const }];
    const onChange = vi.fn();
    render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters, dateMode: 'utc', onChange, onClear: vi.fn() });
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    await fireEvent.change(screen.getByLabelText('From (your time zone)'), { target: { value: '2026-07-01' } });
    expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'after', values: ['2026-06-30T23:00:00.000Z'], calendar: 'local' }]);
    await fireEvent.change(screen.getByLabelText('To (UTC)'), { target: { value: '2026-12-31' } });
    expect(onChange).toHaveBeenLastCalledWith([filters[0], { dimension: 'before', values: ['2027-01-01T00:00:00.000Z'] }]);
  } finally { vi.unstubAllEnvs(); }
});

it.each([
  ['Asia/Tokyo', '2024-11-30T15:00:00.000Z', '2024-12-31T15:00:00.000Z', '2024-12-14T15:00:00.000Z', '2024-12-20T15:00:00.000Z'],
  ['America/New_York', '2024-12-01T05:00:00.000Z', '2025-01-01T05:00:00.000Z', '2024-12-15T05:00:00.000Z', '2024-12-21T05:00:00.000Z'],
])('edits local December bounds in %s', async (zone, after, before, editedAfter, editedBefore) => {
  vi.stubEnv('TZ', zone);
  try {
    const onChange = vi.fn();
    const filters = [{ dimension: 'after' as const, values: [after], calendar: 'local' as const }, { dimension: 'before' as const, values: [before], calendar: 'local' as const }];
    render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters, dateMode: 'auto', onChange, onClear: vi.fn() });
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    expect((screen.getByLabelText('From') as HTMLInputElement).value).toBe('2024-12-01');
    expect((screen.getByLabelText('To') as HTMLInputElement).value).toBe('2024-12-31');
    await fireEvent.change(screen.getByLabelText('From'), { target: { value: '2024-12-15' } });
    expect(onChange).toHaveBeenLastCalledWith([filters[1], { dimension: 'after', values: [editedAfter], calendar: 'local' }]);
    await fireEvent.change(screen.getByLabelText('To'), { target: { value: '2024-12-20' } });
    expect(onChange).toHaveBeenLastCalledWith([filters[0], { dimension: 'before', values: [editedBefore], calendar: 'local' }]);
  } finally { vi.unstubAllEnvs(); }
});

it('ends a local date at the next midnight across daylight saving', async () => {
  vi.stubEnv('TZ', 'America/New_York');
  try {
    const onChange = vi.fn();
    render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters: [], dateMode: 'auto', onChange, onClear: vi.fn() });
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    await fireEvent.change(screen.getByLabelText('To'), { target: { value: '2024-03-10' } });
    expect(onChange).toHaveBeenLastCalledWith([{ dimension: 'before', values: ['2024-03-11T04:00:00.000Z'], calendar: 'local' }]);
  } finally { vi.unstubAllEnvs(); }
});

it('keeps each calendar when the bounds use different time zones', async () => {
  vi.stubEnv('TZ', 'America/New_York');
  try {
    const onChange = vi.fn();
    const filters = [{ dimension: 'after' as const, values: ['2024-03-01T00:00:00.000Z'] }, { dimension: 'before' as const, values: ['2024-04-01T04:00:00.000Z'], calendar: 'local' as const }];
    const props = { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters, dateMode: 'auto' as const, onChange, onClear: vi.fn() };
    const view = render(FilterMenu, props);
    await fireEvent.click(screen.getByRole('button', { name: 'Filter' }));
    expect((screen.getByLabelText('From (UTC)') as HTMLInputElement).value).toBe('2024-03-01');
    expect((screen.getByLabelText('To (your time zone)') as HTMLInputElement).value).toBe('2024-03-31');
    await fireEvent.change(screen.getByLabelText('From (UTC)'), { target: { value: '2024-03-02' } });
    expect(onChange).toHaveBeenLastCalledWith([filters[1], { dimension: 'after', values: ['2024-03-02T00:00:00.000Z'] }]);
    await fireEvent.change(screen.getByLabelText('To (your time zone)'), { target: { value: '2024-03-10' } });
    expect(onChange).toHaveBeenLastCalledWith([filters[0], { dimension: 'before', values: ['2024-03-11T04:00:00.000Z'], calendar: 'local' }]);
    const utcFilters = [{ dimension: 'after' as const, values: ['2026-01-01T00:00:00.000Z'] }, { dimension: 'before' as const, values: ['2026-02-01T00:00:00.000Z'] }];
    await view.rerender({ ...props, filters: utcFilters });
    expect(screen.getByText('Dates (UTC)')).toBeDefined();
    expect((screen.getByLabelText('From') as HTMLInputElement).value).toBe('2026-01-01');
    expect((screen.getByLabelText('To') as HTMLInputElement).value).toBe('2026-01-31');
    await fireEvent.change(screen.getByLabelText('From'), { target: { value: '2026-01-02' } });
    expect(onChange).toHaveBeenLastCalledWith([utcFilters[1], { dimension: 'after', values: ['2026-01-02T00:00:00.000Z'] }]);
    await view.rerender({ ...props, filters: [utcFilters[0]!] });
    await fireEvent.change(screen.getByLabelText('To'), { target: { value: '2026-01-15' } });
    expect(onChange).toHaveBeenLastCalledWith([utcFilters[0], { dimension: 'before', values: ['2026-01-16T00:00:00.000Z'] }]);
  } finally { vi.unstubAllEnvs(); }
});

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
  render(FilterMenu, { client: createAPIClient(async () => Response.json({ rows: [], accounts: [] })), filters, onChange, onClear: vi.fn() });
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
