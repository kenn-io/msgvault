import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createAPIClient } from '../../api/client';
import PersonAgenda from './PersonAgenda.svelte';

const schedule = { uid: 'job-one', definition_event_uid: 'viewed-event', state: 'active', enabled: true, deleted: false, editable: true, parent_closed: false, summary: 'Weekly on Monday at 09:00', timezone: 'Europe/London', trigger_kind: 'cron', cron: '0 9 * * 1', interval_seconds: 0, grace_seconds: 0, catchup: 'all', recipient: 'synthetic/inbox', occurrence_title: 'Review notes' };
const agenda = { project: 'example', truncated: false, recurrence: { state: 'ready' }, items: [{ uid: 'parent', ref: 'parent-ref', title: 'Weekly review', list: 'agenda', state: 'open', status: 'open', kind: 'series', schedules: [schedule], occurrences_complete: true, open_occurrences: 0 }] };

function setup(write: (request: Request) => Promise<Response>) {
  const writes: Request[] = [];
  const client = createAPIClient(vi.fn<typeof fetch>(async input => {
    const request = input instanceof Request ? input : new Request(input);
    if (request.url.endsWith('/integrations/kata/status')) return Response.json({ state: 'ready' });
    if (request.method === 'POST') { writes.push(request); return write(request); }
    return Response.json(agenda);
  }));
  return { client, writes };
}

describe('PersonAgenda schedule management', () => {
  it('waits for agenda identity before opening recurring creation', async () => {
    let resolve!: (response: Response) => void;
    const client = createAPIClient(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.url.endsWith('/integrations/kata/status')) return Response.json({ state: 'ready' });
      return new Promise<Response>(done => { resolve = done; });
    }));
    render(PersonAgenda, { client, personID: 7 });
    const button = await screen.findByRole('button', { name: 'Add recurring task' });
    expect(button).toHaveProperty('disabled', true);
    resolve(Response.json({ ...agenda, person_uid: 'person-seven', person_uids: ['person-seven'], recurring_target: 'target-one' }));
    await waitFor(() => expect(button).toHaveProperty('disabled', false));
  });

  it('requires lifecycle review and sends the viewed job event under the person and parent', async () => {
    const { client, writes } = setup(async () => Response.json({ schedule: { ...schedule, state: 'paused', enabled: false, definition_event_uid: 'confirmed-event' } }));
    render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Manage schedule Weekly on Monday at 09:00' }));
    expect(screen.getByText(/synthetic\/inbox/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: /^Pause schedule$/ }));
    expect(writes).toHaveLength(0);
    expect(screen.getByText(/Existing occurrences and inbox requests remain/)).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: /^Pause$/ }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(new URL(writes[0].url).pathname).toBe('/api/v1/people/7/agenda/parent/schedules/job-one/actions');
    expect(await writes[0].clone().json()).toEqual({ action: 'pause', expected_event_uid: 'viewed-event' });
    await screen.findByText('Paused');
  });

  it('retains edit fields and requires a refresh after a stale-event conflict', async () => {
    const { client, writes } = setup(async () => Response.json({ error: 'schedule_conflict', message: 'The schedule changed. Refresh before trying again.' }, { status: 409 }));
    render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Manage schedule Weekly on Monday at 09:00' }));
    await fireEvent.click(screen.getByRole('button', { name: /^Edit schedule$/ }));
    await fireEvent.input(screen.getByRole('textbox', { name: 'Occurrence title' }), { target: { value: 'Changed notes' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Review schedule changes' }));
    expect(writes).toHaveLength(0);
    expect(screen.getByRole('combobox', { name: 'Missed occurrences' })).toHaveProperty('value', 'all');
    await fireEvent.input(screen.getByRole('textbox', { name: 'Inbox recipient' }), { target: { value: 'other/inbox' } });
    expect(screen.queryByRole('button', { name: /^Save$/ })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Review schedule changes' }));
    expect(screen.getByRole('textbox', { name: 'Inbox recipient' })).toHaveProperty('value', 'other/inbox');
    await fireEvent.click(screen.getByRole('button', { name: /^Save$/ }));
    await screen.findByText(/The schedule changed/);
    expect(screen.getByRole('textbox', { name: 'Occurrence title' })).toHaveProperty('value', 'Changed notes');
    expect(screen.getByRole('button', { name: 'Review schedule changes' })).toHaveProperty('disabled', true);
  });

  it('discards a schedule result after switching to another person', async () => {
    let resolve!: (response: Response) => void;
    const { client } = setup(() => new Promise<Response>(done => { resolve = done; }));
    const view = render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Manage schedule Weekly on Monday at 09:00' }));
    await fireEvent.click(screen.getByRole('button', { name: /^Pause schedule$/ }));
    await fireEvent.click(screen.getByRole('button', { name: /^Pause$/ }));
    await view.rerender({ client, personID: 8 });
    await screen.findByText('Weekly review');
    const response = Response.json({ schedule: { ...schedule, state: 'paused', enabled: false } });
    const read = vi.spyOn(response, 'text');
    resolve(response);
    await waitFor(() => expect(read).toHaveBeenCalled());
    await read.mock.results[0].value;
    await tick();
    expect(screen.queryByText('Paused')).toBeNull();
  });

  it.each(['fetch', 'body'])('requires refresh when the schedule %s is lost', async failure => {
    const { client } = setup(async () => {
      if (failure === 'fetch') throw new Error('connection lost');
      const response = Response.json({ schedule });
      vi.spyOn(response, 'text').mockRejectedValue(new Error('body lost'));
      return response;
    });
    render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Manage schedule Weekly on Monday at 09:00' }));
    await fireEvent.click(screen.getByRole('button', { name: /^Pause schedule$/ }));
    await fireEvent.click(screen.getByRole('button', { name: /^Pause$/ }));
    await screen.findByRole('alert');
    expect(screen.getByRole('button', { name: /^Pause$/ })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Refresh schedule' })).toBeDefined();
  });

  it('discloses catch-up and inbox routing before a lifecycle change', async () => {
    const { client, writes } = setup(async () => Response.json({ schedule }));
    render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Manage schedule Weekly on Monday at 09:00' }));
    expect(screen.getByText(/Kata sends inbox requests to synthetic\/inbox/)).toBeDefined();
    expect(screen.getByText(/Resuming or restoring may deliver missed occurrences/)).toBeDefined();
    expect(writes).toHaveLength(0);
  });
});
