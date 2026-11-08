import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createAPIClient } from '../../api/client';
import RecurringTaskForm from './RecurringTaskForm.svelte';

beforeEach(() => {
  const held = new Set<string>();
  Object.defineProperty(navigator, 'locks', {
    configurable: true, value: { request: async (name: string, _options: LockOptions, callback: (lock: Lock | null) => Promise<void>) => {
      if (held.has(name)) return callback(null);
      held.add(name);
      try { return await callback({ name, mode: 'exclusive' }); } finally { held.delete(name); }
    } },
  });
});
afterEach(() => { vi.restoreAllMocks(); Reflect.deleteProperty(navigator, 'locks'); localStorage.clear(); });

describe('RecurringTaskForm', () => {
  it('retains the draft when the form is closed', async () => {
    const view = render(RecurringTaskForm, { client: createAPIClient(vi.fn<typeof fetch>()), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByRole('textbox', { name: 'Recurring task title' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring task title' }), { target: { value: 'Review notes' } });
    view.unmount();
    expect(JSON.parse(localStorage.getItem('msgvault.recurring-draft.uid.person-seven') ?? '{}').title).toBe('Review notes');
  });

  it('preserves an unreadable saved request and does not create a replacement', async () => {
    localStorage.setItem('msgvault.recurring-draft.uid.person-seven', '{broken');
    const fetchMock = vi.fn<typeof fetch>();
    render(RecurringTaskForm, { client: createAPIClient(fetchMock), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByText(/cannot load saved drafts for this person/i);
    expect(screen.queryByRole('button', { name: 'Create recurring task' })).toBeNull();
    expect(localStorage.getItem('msgvault.recurring-draft.uid.person-seven')).toBe('{broken');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('keeps an oversized first request editable and sends it after the notes are shortened', async () => {
    const fetchMock = vi.fn<typeof fetch>(async () => new Response(null, { status: 503 }));
    render(RecurringTaskForm, { client: createAPIClient(fetchMock), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByRole('textbox', { name: 'Recurring task title' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring task title' }), { target: { value: 'Review notes' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Parent task owner' }), { target: { value: 'synthetic' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring occurrence title' }), { target: { value: 'Review' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring inbox recipient' }), { target: { value: 'synthetic/inbox' } });
    await fireEvent.change(screen.getByRole('combobox', { name: 'Recurring missed occurrences' }), { target: { value: 'latest' } });
    const notes = screen.getByRole('textbox', { name: 'Parent task notes' });
    await fireEvent.input(notes, { target: { value: 'é'.repeat(32768) } });
    await fireEvent.click(screen.getByRole('button', { name: 'Review recurring task' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create recurring task' }));
    expect(screen.getByText(/too large/i)).toBeDefined();
    expect(notes).toHaveProperty('disabled', false);
    expect(fetchMock).not.toHaveBeenCalled();
    await fireEvent.input(notes, { target: { value: 'Short notes' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create recurring task' }));
    await screen.findByText(/outcome is unknown/i);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(notes).toHaveProperty('disabled', true);
  });

  it('keeps the first draft editable when saving its request identity fails', async () => {
    const fetchMock = vi.fn<typeof fetch>();
    const save = Storage.prototype.setItem;
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation((key, value) => {
      if (key === 'msgvault.recurring-draft.uid.person-seven' && JSON.parse(value).attempted) throw new Error('quota');
      return save.call(localStorage, key, value);
    });
    render(RecurringTaskForm, { client: createAPIClient(fetchMock), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByRole('textbox', { name: 'Recurring task title' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring task title' }), { target: { value: 'Review notes' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Parent task owner' }), { target: { value: 'synthetic' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring occurrence title' }), { target: { value: 'Review' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring inbox recipient' }), { target: { value: 'synthetic/inbox' } });
    await fireEvent.change(screen.getByRole('combobox', { name: 'Recurring missed occurrences' }), { target: { value: 'latest' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Review recurring task' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create recurring task' }));
    expect(screen.getByText(/No schedule was created/)).toBeDefined();
    expect(screen.getByRole('textbox', { name: 'Recurring task title' })).toHaveProperty('disabled', false);
    expect(screen.getByRole('textbox', { name: 'Recurring task title' })).toHaveProperty('value', 'Review notes');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('does not create until review and retains the same request after an unknown outcome', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      return requests.length === 1
        ? new Response(null, { status: 503 })
        : Response.json({ error: 'invalid_schedule', message: 'Schedule is invalid' }, { status: 422 });
    }));
    render(RecurringTaskForm, { client, personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByRole('textbox', { name: 'Recurring task title' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring task title' }), { target: { value: 'Review notes' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Parent task owner' }), { target: { value: 'synthetic' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring occurrence title' }), { target: { value: 'Review' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring inbox recipient' }), { target: { value: 'synthetic/inbox' } });
    await fireEvent.change(screen.getByRole('combobox', { name: 'Recurring missed occurrences' }), { target: { value: 'latest' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Review recurring task' }));
    expect(requests).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Create recurring task' }));
    await screen.findByText(/outcome is unknown/i);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry recurring task' }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[0].headers.get('Idempotency-Key')).toBeTruthy();
    expect(requests[1].headers.get('Idempotency-Key')).toBe(requests[0].headers.get('Idempotency-Key'));
    await screen.findByText('Schedule is invalid');
    expect(screen.getByRole('textbox', { name: 'Recurring task title' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Retry recurring task' })).toBeDefined();
  });

  it('allows correcting a rejected first retry key', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ error: 'invalid_idempotency_key', message: 'Check the device clock' }, { status: 422 })));
    render(RecurringTaskForm, { client, personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByRole('textbox', { name: 'Recurring task title' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring task title' }), { target: { value: 'Review notes' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Parent task owner' }), { target: { value: 'synthetic' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring occurrence title' }), { target: { value: 'Review' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring inbox recipient' }), { target: { value: 'synthetic/inbox' } });
    await fireEvent.change(screen.getByRole('combobox', { name: 'Recurring missed occurrences' }), { target: { value: 'latest' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Review recurring task' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create recurring task' }));
    await screen.findByText('Check the device clock');
    expect(screen.getByRole('textbox', { name: 'Recurring task title' })).toHaveProperty('disabled', false);
    expect(screen.getByRole('button', { name: 'Create recurring task' })).toBeDefined();
  });

  it('blocks another tab and preserves the attempted identity when ownership transfers', async () => {
    const fetchMock = vi.fn<typeof fetch>(async () => new Response(null, { status: 503 }));
    const props = { client: createAPIClient(fetchMock), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() };
    const first = render(RecurringTaskForm, props);
    await screen.findByRole('textbox', { name: 'Recurring task title' });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring task title' }), { target: { value: 'Review notes' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Parent task owner' }), { target: { value: 'synthetic' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring occurrence title' }), { target: { value: 'Review' } });
    await fireEvent.input(screen.getByRole('textbox', { name: 'Recurring inbox recipient' }), { target: { value: 'synthetic/inbox' } });
    await fireEvent.change(screen.getByRole('combobox', { name: 'Recurring missed occurrences' }), { target: { value: 'latest' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Review recurring task' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Create recurring task' }));
    await screen.findByText(/outcome is unknown/i);
    const saved = localStorage.getItem('msgvault.recurring-draft.uid.person-seven');
    const second = render(RecurringTaskForm, props);
    await within(second.container).findByText(/open in another tab/i);
    second.unmount();
    expect(localStorage.getItem('msgvault.recurring-draft.uid.person-seven')).toBe(saved);
    first.unmount();
    await waitFor(() => expect(localStorage.getItem('msgvault.recurring-draft.uid.person-seven')).toBe(saved));
    await Promise.resolve();
    await Promise.resolve();
    const next = render(RecurringTaskForm, { ...props, personID: 8, personUIDs: ['survivor', 'person-seven'] });
    await within(next.container).findByRole('button', { name: 'Retry recurring task' });
    expect(within(next.container).getByRole('textbox', { name: 'Recurring task title' })).toHaveProperty('disabled', true);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    await fireEvent.click(within(next.container).getByRole('button', { name: 'Retry recurring task' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(localStorage.getItem('msgvault.recurring-draft.uid.person-seven')).toBe(saved);
  });

  it('migrates an older numeric draft before the person is merged', async () => {
    const legacy = { key: '2026-11-08T12:00:00Z/550e8400-e29b-41d4-a716-446655440000', attempted: true, project: 'example', target: 'target-one', title: 'Review', body: '', list: 'agenda', owner: 'synthetic', settings: { trigger_kind: 'cron', cron: '0 9 * * *', interval_seconds: 60, timezone: 'UTC', occurrence_title: 'Review', occurrence_body: '', recipient: 'synthetic', catchup: 'latest', grace_seconds: 0 } };
    localStorage.setItem('msgvault.recurring-draft.7', JSON.stringify(legacy));
    render(RecurringTaskForm, { client: createAPIClient(vi.fn<typeof fetch>()), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByRole('button', { name: 'Retry recurring task' });
    expect(JSON.parse(localStorage.getItem('msgvault.recurring-draft.uid.person-seven') ?? '{}').key).toBe(legacy.key);
    expect(localStorage.getItem('msgvault.recurring-draft.7')).toBeNull();
  });

  it('blocks a new request when an older unresolved numeric draft cannot be attributed', async () => {
    const legacy = { key: '2026-11-08T12:00:00Z/550e8400-e29b-41d4-a716-446655440000', attempted: true, project: 'example', target: 'target-one', title: 'Review', body: '', list: 'agenda', owner: 'synthetic', settings: { trigger_kind: 'cron', cron: '', interval_seconds: 60, timezone: 'UTC', occurrence_title: 'Review', occurrence_body: '', recipient: 'synthetic', catchup: 'latest', grace_seconds: 0 } };
    localStorage.setItem('msgvault.recurring-draft.7', JSON.stringify(legacy));
    const fetchMock = vi.fn<typeof fetch>();
    render(RecurringTaskForm, { client: createAPIClient(fetchMock), personID: 8, personUIDs: ['survivor', 'person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByText(/cannot load saved drafts for this person/i);
    expect(localStorage.getItem('msgvault.recurring-draft.7')).toBe(JSON.stringify(legacy));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('allows selecting one of two merged drafts without losing either request', async () => {
    const saved = { key: '2026-11-08T12:00:00Z/550e8400-e29b-41d4-a716-446655440000', attempted: true, project: 'example', target: 'target-one', title: 'First', body: '', list: 'agenda', owner: 'synthetic', settings: { trigger_kind: 'cron', cron: '', interval_seconds: 60, timezone: 'UTC', occurrence_title: 'Review', occurrence_body: '', recipient: 'synthetic', catchup: 'latest', grace_seconds: 0 } };
    localStorage.setItem('msgvault.recurring-draft.uid.first', JSON.stringify(saved));
    localStorage.setItem('msgvault.recurring-draft.uid.second', JSON.stringify({ ...saved, key: `${saved.key}-other`, title: 'Second' }));
    const view = render(RecurringTaskForm, { client: createAPIClient(vi.fn<typeof fetch>()), personID: 8, personUIDs: ['first', 'second'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await fireEvent.click(await screen.findByRole('button', { name: 'Resume draft: Second' }));
    expect(screen.getByRole('textbox', { name: 'Recurring task title' })).toHaveProperty('value', 'Second');
    view.unmount();
    expect(JSON.parse(localStorage.getItem('msgvault.recurring-draft.uid.first') ?? '{}').key).toBe(saved.key);
    expect(JSON.parse(localStorage.getItem('msgvault.recurring-draft.uid.second') ?? '{}').title).toBe('Second');
  });

  it('does not retry a saved request against a changed project', async () => {
    const saved = { key: '2026-11-08T12:00:00Z/550e8400-e29b-41d4-a716-446655440000', attempted: true, project: 'old-project', target: 'target-one', title: 'Review', body: '', list: 'agenda', owner: 'synthetic', settings: { trigger_kind: 'cron', cron: '', interval_seconds: 60, timezone: 'UTC', occurrence_title: 'Review', occurrence_body: '', recipient: 'synthetic', catchup: 'latest', grace_seconds: 0 } };
    localStorage.setItem('msgvault.recurring-draft.uid.person-seven', JSON.stringify(saved));
    const fetchMock = vi.fn<typeof fetch>();
    render(RecurringTaskForm, { client: createAPIClient(fetchMock), personID: 7, personUIDs: ['person-seven'], project: 'new-project', target: 'target-two', onCreated: vi.fn(), onClose: vi.fn() });
    await fireEvent.click(await screen.findByRole('button', { name: 'Retry recurring task' }));
    expect(screen.getByText(/original Kata project is unavailable or changed/i)).toBeDefined();
    expect(localStorage.getItem('msgvault.recurring-draft.uid.person-seven')).toBe(JSON.stringify(saved));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('refuses creation if browser coordination is unavailable', async () => {
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
    const fetchMock = vi.fn<typeof fetch>();
    render(RecurringTaskForm, { client: createAPIClient(fetchMock), personID: 7, personUIDs: ['person-seven'], project: 'example', target: 'target-one', onCreated: vi.fn(), onClose: vi.fn() });
    await screen.findByText(/use a supported browser/i);
    expect(screen.queryByRole('button', { name: 'Create recurring task' })).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
