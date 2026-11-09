import { openTypeahead } from '../../../test/kit-ui';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import PersonAgenda from './PersonAgenda.svelte';

const STATUS_PATH = '/api/v1/integrations/kata/status';

function isStatus(request: Request): boolean {
  return new URL(request.url).pathname === STATUS_PATH;
}

function readyStatus(): Response {
  return Response.json({ state: 'ready' });
}

/** Requires a closed form and opens it once the agenda can take a task. */
async function openForm(): Promise<void> {
  const add = await screen.findByRole('button', { name: 'Add task' });
  expect(screen.queryByLabelText('New task')).toBeNull();
  await fireEvent.click(add);
}

/** Requires an open form and types a task title. */
async function typeTask(value: string): Promise<void> {
  const input = screen.getByLabelText('New task');
  await waitFor(() => expect(input).toHaveProperty('disabled', false));
  await fireEvent.input(input, { target: { value } });
}

describe('PersonAgenda', () => {
  it.each([{ input: null, list: 'waiting' }, { input: 'Gift  Ideas', list: 'gift ideas' }])("shows each task's list and creates into $list", async ({ input, list }) => {
    const requests: Request[] = [];
    const createdItems: ReturnType<typeof item>[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        const { title, list } = await request.clone().json();
        const created = { ...item(`new-${createdItems.length}`, title), list };
        createdItems.push(created);
        return Response.json({ item: created }, { status: 201 });
      }
      return Response.json({ project: 'msgvault', items: [
        { ...item('one', 'Send the revised budget'), web_url: 'https://tasks.example.test/one' },
        { ...item('two', 'Renewal call'), list: 'waiting' },
        ...createdItems
      ] });
    }));

    render(PersonAgenda, { client, personID: 7 });

    expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
    expect(await screen.findByText('waiting')).toBeDefined();
    expect(screen.getByRole('link', { name: 'Send the revised budget' }).getAttribute('href')).toBe('https://tasks.example.test/one');

    await openForm();
    await typeTask('Send notes');
    await openTypeahead('List');
    if (input) {
      await fireEvent.input(screen.getByRole('combobox', { name: 'List' }), { target: { value: input } });
      await fireEvent.keyDown(screen.getByRole('combobox', { name: 'List' }), { key: 'Enter' });
    } else await fireEvent.mouseDown(screen.getByRole('option', { name: 'waiting' }));
    await screen.findByRole('button', { name: `List: ${list}` });
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(0);
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(screen.getByText('Send notes')).toBeDefined());

    const post = requests.find(request => request.method === 'POST');
    expect(post).toBeDefined();
    expect(await post!.clone().json()).toMatchObject({ title: 'Send notes', list });
    await openTypeahead('List');
    expect(screen.getAllByRole('option', { name: 'waiting' })).toHaveLength(1);
  });

  it.each([
    { outcome: 'pending', replayCount: 2, changedCount: 3 },
    { outcome: 'failed', replayCount: 3, changedCount: 4 }
  ] as const)('keeps a $outcome create retry key across profile revisions and rotates it when the payload changes', async ({ outcome, replayCount, changedCount }) => {
    const requests: Request[] = [];
    let attempts = 0;
    let releasePost: ((response: Response) => void) | undefined;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        attempts += 1;
        if (attempts === 1 && outcome === 'pending') {
          return new Promise<Response>(resolve => { releasePost = resolve; });
        }
        if (attempts < 3) {
          return Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 });
        }
        return Response.json({ item: item('new', 'Send notes') }, { status: 201 });
      }
      return Response.json({ project: 'msgvault', items: attempts >= 3 ? [item('new', 'Send notes')] : [] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7, refreshKey: '1' });
    await screen.findByText('No tasks yet');

    await openForm();
    await typeTask('Send notes');
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(requests.filter(request => request.method === 'POST')).toHaveLength(1));
    await expect(requests.find(request => request.method === 'POST')!.clone().json()).resolves.toMatchObject({ title: 'Send notes', list: 'agenda' });
    if (outcome === 'pending') {
      await waitFor(() => expect(releasePost).toBeDefined());
      await rerender({ client, personID: 7, refreshKey: '2' });
      expect(requests.filter(isStatus)).toHaveLength(1);
      releasePost!(Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 }));
    } else {
      expect(await screen.findByText("Couldn't add the task.")).toBeDefined();
      await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
      await waitFor(() => expect(requests.filter(request => request.method === 'POST')).toHaveLength(2));
      expect(await requests.filter(request => request.method === 'POST')[1]!.clone().json()).toMatchObject({ title: 'Send notes', list: 'agenda' });
      await rerender({ client, personID: 7, refreshKey: '2' });
    }
    await waitFor(() => expect(requests.filter(request => request.method === 'GET' && new URL(request.url).pathname === '/api/v1/people/7/agenda')).toHaveLength(2));
    await screen.findByText('No tasks yet');
    expect(screen.getByLabelText('New task')).toHaveProperty('value', 'Send notes');

    // The profile revision changed, but the submission stayed the same.
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(replayCount));
    const keys = requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key'));
    expect(keys.length).toBe(replayCount);
    expect(keys[0]).toBeTruthy();
    expect(keys.every(key => key === keys[0])).toBe(true);

    // Changing the title mints a fresh key instead of replaying the old one.
    await typeTask('Send notes tomorrow');
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(changedCount));
    const rotated = requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key'));
    expect(rotated.length).toBe(changedCount);
    expect(rotated.at(-1)).not.toBe(rotated[0]);
    await waitFor(() => expect(screen.getByText('Send notes')).toBeDefined());
  });

  it.each(['create', 'unlink'] as const)('defers profile refreshes during a pending %s and applies its completion', async operation => {
    const onAnnounce = vi.fn();
    const requests: Request[] = [];
    let releaseWrite!: (response: Response) => void;
    let settled = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST' || request.method === 'DELETE') {
        return new Promise<Response>(resolve => { releaseWrite = resolve; });
      }
      return Response.json({ project: 'msgvault', items: settled
        ? [item('merged', 'Merged task'), ...(operation === 'create' ? [item('new', 'Send notes')] : [])]
        : [item('one', 'Ask about launch')]
      });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7, refreshKey: '1', onAnnounce });
    await screen.findByText('Ask about launch');
    await openForm();
    await typeTask('Send notes');
    await openTypeahead('List');
    await fireEvent.input(screen.getByRole('combobox', { name: 'List' }), { target: { value: 'Follow-up' } });
    await fireEvent.keyDown(screen.getByRole('combobox', { name: 'List' }), { key: 'Enter' });
    await fireEvent.click(screen.getByRole('button', { name: operation === 'create' ? 'Add task' : 'Unlink Ask about launch' }));
    await waitFor(() => expect(releaseWrite).toBeDefined());

    await rerender({ client, personID: 7, refreshKey: '2', onAnnounce });
    await rerender({ client, personID: 7, refreshKey: '3', onAnnounce });
    expect(requests.filter(isStatus)).toHaveLength(1);
    expect(screen.getByLabelText('New task')).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'List: follow-up' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Add task' })).toHaveProperty('disabled', true);
    expect(screen.getByRole('button', { name: 'Unlink Ask about launch' })).toHaveProperty('disabled', true);
    expect(onAnnounce).not.toHaveBeenCalled();

    settled = true;
    releaseWrite(Response.json({ item: operation === 'create' ? item('new', 'Send notes') : item('one', 'Ask about launch') }));
    await screen.findByText('Merged task');
    await waitFor(() => expect(screen.getByLabelText('New task')).toHaveProperty('disabled', false));
    expect(screen.getByRole('button', { name: 'List: follow-up' })).toBeDefined();
    expect(onAnnounce).toHaveBeenCalledExactlyOnceWith(operation === 'create' ? 'Task added.' : 'Task unlinked.');
    expect(screen.getByLabelText('New task')).toHaveProperty('value', operation === 'create' ? '' : 'Send notes');
    if (operation === 'create') expect(screen.getByText('Send notes')).toBeDefined();
    expect(screen.queryByText('Ask about launch')).toBeNull();
  });

  it('resets the idempotency key when the selected person changes', async () => {
    const requests: Request[] = [];
    let failedOnce = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        if (!failedOnce) {
          failedOnce = true;
          return Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 });
        }
        return Response.json({ item: item('new', 'Send notes') }, { status: 201 });
      }
      return Response.json({ project: 'msgvault', items: [] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    await screen.findByText('No tasks yet');

    await openForm();
    await typeTask('Send notes');
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    expect(await screen.findByText('Unable to add agenda item')).toBeDefined();

    await rerender({ client, personID: 8 });
    await waitFor(() => expect(requests.some((request) => new URL(request.url).pathname === '/api/v1/people/8/agenda')).toBe(true));
    await screen.findByText('No tasks yet');

    await openForm();
    await typeTask('Send notes');
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(2));

    const posts = requests.filter((request) => request.method === 'POST');
    expect(new URL(posts[0].url).pathname).toBe('/api/v1/people/7/agenda');
    expect(new URL(posts[1].url).pathname).toBe('/api/v1/people/8/agenda');
    expect(posts[1].headers.get('Idempotency-Key')).not.toBe(posts[0].headers.get('Idempotency-Key'));
  });

  it.each([
    ['authentication_required', 'Reconnect Kata to manage agenda items'],
    ['incompatible', 'Kata API is incompatible.'],
  ] as const)('gates agenda mutations when the integration is %s', async (state, message) => {
    let ready = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) {
        return ready
          ? readyStatus()
          : Response.json({ state, message });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText(`Kata integration ${state}: ${message}`)).toBeDefined();
    // The gated agenda never requests the 503-prone list route, so no item
    // rows or Unlink controls render; the status message is the only notice.
    expect(screen.queryByRole('button', { name: 'Unlink Ask' })).toBeNull();
    expect(screen.queryByText('Task service is unavailable')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Add task' })).toBeNull();

    ready = true;
    await rerender({ client, personID: 8 });
    await openForm();
    await waitFor(() => expect((screen.getByLabelText('New task') as HTMLInputElement).disabled).toBe(false));
    expect(screen.queryByText(`Kata integration ${state}: ${message}`)).toBeNull();
    // The switch cleared person A's rows; person 8's list serves the same
    // item, so wait for it to render before asserting the Unlink gate.
    await screen.findByRole('button', { name: 'Unlink Ask' });
    await typeTask('Send notes');
    expect(screen.getByRole('button', { name: 'List: agenda' })).toHaveProperty('disabled', false);
    expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Unlink Ask' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('shows the integration status instead of the add form when the integration is disabled', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return Response.json({ state: 'disabled', message: 'Kata is not configured.' });
      throw new Error(`Unexpected ${request.method} ${request.url}`);
    }));

    render(PersonAgenda, { client, personID: 7 });

    const status = await screen.findByText('Kata integration disabled: Kata is not configured.');
    expect(status.getAttribute('role')).toBe('status');
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Add task' })).toBeNull();
  });

  it('does not request the agenda while the task integration is not ready', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) {
        return Response.json({ state: 'authentication_required', message: 'Reconnect Kata to manage agenda items' });
      }
      return Response.json({ error: 'task_integration_unavailable', message: 'Task service is unavailable' }, { status: 503 });
    }));

    render(PersonAgenda, { client, personID: 7 });

    expect(await screen.findByText('Kata integration authentication_required: Reconnect Kata to manage agenda items')).toBeDefined();
    await waitFor(() => expect(screen.queryByText('Loading…')).toBeNull());
    // The status message is the single explanation: the list route 503s for
    // a gated integration, so calling it would only add a redundant error
    // next to the notice and claim an emptiness it never checked.
    expect(screen.queryByText('Task service is unavailable')).toBeNull();
    expect(requests.some((request) => new URL(request.url).pathname === '/api/v1/people/7/agenda')).toBe(false);
    expect(screen.queryByText('No tasks yet')).toBeNull();
  });

  it('does not request the agenda when the Kata service is incompatible', async () => {
    const requests: Request[] = [];
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) {
        return Response.json({ state: 'incompatible', message: 'Kata API is incompatible.' });
      }
      return Response.json({ error: 'task_integration_unavailable', message: 'Task service is unavailable' }, { status: 503 });
    }));

    render(PersonAgenda, { client, personID: 7 });

    expect(await screen.findByText('Kata integration incompatible: Kata API is incompatible.')).toBeDefined();
    await waitFor(() => expect(screen.queryByText('Loading…')).toBeNull());
    expect(screen.queryByText('Task service is unavailable')).toBeNull();
    expect(requests.some((request) => new URL(request.url).pathname === '/api/v1/people/7/agenda')).toBe(false);
    expect(screen.queryByText('No tasks yet')).toBeNull();
  });

  it('keeps the person page usable when Kata is unavailable', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      return Response.json({ error: 'task_integration_unavailable', message: 'Task service is unavailable' }, { status: 503 });
    }));
    render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('Task service is unavailable')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Tasks' })).toBeDefined();
  });

  it('distinguishes an available empty agenda from an unavailable one', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      return Response.json({ project: 'msgvault', items: [] });
    }));
    render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('No tasks yet')).toBeDefined();
    expect(screen.queryByText('Task service is unavailable')).toBeNull();
  });

  it('clears the prior person and disarms mutations before the next person resolves', async () => {
    const requests: Request[] = [];
    let statusCalls = 0;
    let releaseStatus: ((response: Response) => void) | null = null;
    let releaseList: ((response: Response) => void) | null = null;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) {
        statusCalls += 1;
        if (statusCalls === 1) return readyStatus();
        return new Promise<Response>((resolve) => { releaseStatus = resolve; });
      }
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        return new Promise<Response>((resolve) => { releaseList = resolve; });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask about launch')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    const staleUnlink = await screen.findByRole('button', { name: 'Unlink Ask about launch' });
    expect((staleUnlink as HTMLButtonElement).disabled).toBe(false);

    await rerender({ client, personID: 8 });

    // Person A's rows and readiness must not survive into the transition:
    // nothing from A stays clickable while B's status and list are pending.
    expect(screen.queryByText('Ask about launch')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Unlink Ask about launch' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Add task' })).toBeNull();

    // A click that slips through must never pair A's ref with B's person id.
    await fireEvent.click(staleUnlink);
    expect(requests.some((request) => request.method === 'DELETE')).toBe(false);

    // Once B's status and list both resolve, the agenda recovers.
    releaseStatus!(readyStatus());
    await waitFor(() => expect(releaseList).not.toBeNull());
    releaseList!(Response.json({ project: 'msgvault', items: [] }));
    await screen.findByText('No tasks yet');
    await openForm();
    await waitFor(() => expect((screen.getByLabelText('New task') as HTMLInputElement).disabled).toBe(false));
    expect(requests.some((request) => request.method === 'DELETE')).toBe(false);
  });

  it('discards a late response from the prior person after the selection changes', async () => {
    const requests: Request[] = [];
    let releaseAList: ((response: Response) => void) | null = null;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (new URL(request.url).pathname === '/api/v1/people/7/agenda') {
        return new Promise<Response>((resolve) => { releaseAList = resolve; });
      }
      return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    await waitFor(() => expect(releaseAList).not.toBeNull());

    await rerender({ client, personID: 8 });
    await screen.findByText('B item');

    // A's list finally answers after the switch: generation protection must
    // keep it from restoring A's items over B's.
    releaseAList!(Response.json({ project: 'msgvault', items: [item('one', 'Late A item')] }));
    await waitFor(() => expect(requests.length).toBeGreaterThan(0));
    expect(screen.getByText('B item')).toBeDefined();
    expect(screen.queryByText('Late A item')).toBeNull();
  });

  it.each([
    { operation: 'create', outcome: 'failure', method: 'POST', status: 503 },
    { operation: 'create', outcome: 'success', method: 'POST', status: 201 },
    { operation: 'unlink', outcome: 'failure', method: 'DELETE', status: 503 },
    { operation: 'unlink', outcome: 'success', method: 'DELETE', status: 200 }
  ])('keeps the next person draft, list, and notices after a prior $operation $outcome', async ({ operation, outcome, method, status }) => {
    const onAnnounce = vi.fn();
    let releaseWrite!: (response: Response) => void;
    let personEightLists = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async input => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      if (request.method === method) return new Promise<Response>(resolve => { releaseWrite = resolve; });
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        personEightLists += 1;
        return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask about launch')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7, onAnnounce });
    await screen.findByText('Ask about launch');
    if (operation === 'create') {
      await openForm();
      await typeTask('A note');
      await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    } else await fireEvent.click(screen.getByRole('button', { name: 'Unlink Ask about launch' }));
    await waitFor(() => expect(releaseWrite).toBeDefined());

    await rerender({ client, personID: 8, onAnnounce });
    await screen.findByText('B item');
    await openForm();
    await typeTask('B note');
    await act(async () => { releaseWrite(outcome === 'failure'
      ? Response.json({ error: 'task_unavailable', message: `Unable to ${operation} agenda item` }, { status })
      : Response.json({ item: item('one', 'A note') }, { status })); });
    expect(screen.getByLabelText('New task')).toHaveProperty('disabled', false);
    expect(screen.getByRole('button', { name: 'Add task' })).toHaveProperty('disabled', false);
    expect(screen.getByLabelText('New task')).toHaveProperty('value', 'B note');
    expect(screen.getByText('B item')).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(onAnnounce).not.toHaveBeenCalled();
    expect(personEightLists).toBe(1);
  });

  it('releases the form for the next person while the prior mutation is still in flight', async () => {
    const releases: Array<(response: Response) => void> = [];
    let released = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        return new Promise<Response>((resolve) => {
          releases.push((response) => {
            released += 1;
            resolve(response);
          });
        });
      }
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
      }
      return Response.json({ project: 'msgvault', items: [] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    await screen.findByText('No tasks yet');
    await openForm();
    await typeTask('A note');
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(releases.length).toBe(1)); // A's create hangs.

    // Person B is selected while A's create still hangs: B's form must be
    // usable immediately, not locked behind a request that may never settle.
    await rerender({ client, personID: 8 });
    await screen.findByText('B item');
    await openForm();
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(false));

    // B starts a create; A's hung create then fails. The stale completion
    // must leave B's in-flight mutation alone instead of unlocking the form
    // while B's own request is still pending.
    await typeTask('B note');
    await fireEvent.click(screen.getByRole('button', { name: 'Add task' }));
    await waitFor(() => expect(releases.length).toBe(2));
    expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(true);

    releases[0]!(Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 }));
    await waitFor(() => expect(released).toBe(1));
    expect((screen.getByRole('button', { name: 'Add task' }) as HTMLButtonElement).disabled).toBe(true);

    releases[1]!(Response.json({ item: item('new', 'B note') }, { status: 201 }));
    // B's success clears the typed title, so the submit button stays disabled
    // until text is entered again; the inputs are the mutating-only signal.
    await waitFor(() => expect((screen.getByLabelText('New task') as HTMLInputElement).disabled).toBe(false));
  });

  it('unlinks by Kata ref and then reloads the live agenda', async () => {
    const requests: Request[] = [];
    let unlinked = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'DELETE') {
        unlinked = true;
        return Response.json({ item: item('one', 'Ask') });
      }
      return Response.json({ project: 'msgvault', items: unlinked ? [] : [item('one', 'Ask')] });
    }));
    render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Unlink Ask' }));
    await waitFor(() => expect(screen.getByText('No tasks yet')).toBeDefined());
    const request = requests.find((candidate) => candidate.method === 'DELETE');
    expect(new URL(request!.url).pathname).toBe('/api/v1/people/7/agenda/one');
  });

  it('reports that a bounded agenda has more open tasks', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask')], truncated: true });
    }));
    render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('More tasks are linked to this person.')).toBeDefined();
  });
});

function item(ref: string, title: string) {
  return {
    uid: `01${ref.toUpperCase()}`,
    ref,
    qualified_ref: `msgvault#${ref}`,
    project: 'msgvault',
    title,
    revision: '1',
    list: 'agenda',
    status: 'open',
    state: 'open',
  };
}
