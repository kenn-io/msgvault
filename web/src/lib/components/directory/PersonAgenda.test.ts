import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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

describe('PersonAgenda', () => {
  it('groups live tasks into virtual lists and creates with a retry-stable request id', async () => {
    const requests: Request[] = [];
    let created = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        created = true;
        return Response.json({ item: item('new', 'Send notes') }, { status: 201 });
      }
      return Response.json({ project: 'msgvault', items: [
        { ...item('one', 'Ask about launch'), web_url: 'https://tasks.example.test/one' },
        { ...item('two', 'Book idea'), list: 'gift ideas' },
        ...(created ? [item('new', 'Send notes')] : [])
      ] });
    }));

    render(PersonAgenda, { client, personID: 7 });

    expect(screen.getByRole('heading', { name: 'Agenda' })).toBeDefined();
    expect(await screen.findByRole('heading', { name: 'Gift Ideas' })).toBeDefined();
    expect(screen.getByRole('link', { name: 'Open in Kata' }).getAttribute('href')).toBe('https://tasks.example.test/one');

    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(screen.getByText('Send notes')).toBeDefined());

    const post = requests.find((request) => request.method === 'POST');
    expect(post).toBeDefined();
    expect(post!.headers.get('Idempotency-Key')).toBeTruthy();
    expect(await post!.clone().json()).toMatchObject({ title: 'Send notes', list: 'agenda' });
  });

  it('reuses the idempotency key for unchanged retries and rotates it when the payload changes', async () => {
    const requests: Request[] = [];
    let attempts = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        attempts += 1;
        if (attempts < 3) {
          return Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 });
        }
        return Response.json({ item: item('new', 'Send notes') }, { status: 201 });
      }
      return Response.json({ project: 'msgvault', items: attempts >= 3 ? [item('new', 'Send notes')] : [] });
    }));

    render(PersonAgenda, { client, personID: 7 });
    await screen.findByText('No linked Kata items.');

    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    expect(await screen.findByText('Unable to add agenda item')).toBeDefined();

    // Same person, title, and list: an unknown-outcome retry must reuse the key.
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(2));
    const keys = requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key'));
    expect(keys.length).toBe(2);
    expect(keys[0]).toBeTruthy();
    expect(keys[0]).toBe(keys[1]);

    // Changing the title mints a fresh key instead of replaying the old one.
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes tomorrow' } });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(3));
    const rotated = requests.filter((request) => request.method === 'POST').map((request) => request.headers.get('Idempotency-Key'));
    expect(rotated.length).toBe(3);
    expect(rotated[2]).not.toBe(rotated[0]);
    await waitFor(() => expect(screen.getByText('Send notes')).toBeDefined());
  });

  it('canonicalizes the list before fingerprinting so case or spacing changes keep the retry key', async () => {
    const requests: Request[] = [];
    let attempts = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        attempts += 1;
        if (attempts === 1) {
          return Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 });
        }
        return Response.json({ item: item('new', 'Send notes') }, { status: 201 });
      }
      return Response.json({ project: 'msgvault', items: attempts >= 2 ? [item('new', 'Send notes')] : [] });
    }));

    render(PersonAgenda, { client, personID: 7 });
    await screen.findByText('No linked Kata items.');

    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    await fireEvent.input(screen.getByLabelText('List'), { target: { value: 'Gift  Ideas' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    expect(await screen.findByText('Unable to add agenda item')).toBeDefined();

    // The server canonicalizes lists (lowercase, whitespace-collapsed, empty
    // → 'agenda'), so an unknown-outcome retry typed with different case or
    // spacing must keep the same idempotency key and send the canonical list.
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.input(screen.getByLabelText('List'), { target: { value: 'GIFT ideas' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(2));

    const posts = requests.filter((request) => request.method === 'POST');
    const keys = posts.map((request) => request.headers.get('Idempotency-Key'));
    expect(keys[0]).toBeTruthy();
    expect(keys[0]).toBe(keys[1]);
    expect(await posts[0].clone().json()).toMatchObject({ title: 'Send notes', list: 'gift ideas' });
    expect(await posts[1].clone().json()).toMatchObject({ title: 'Send notes', list: 'gift ideas' });
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
    await screen.findByText('No linked Kata items.');

    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    expect(await screen.findByText('Unable to add agenda item')).toBeDefined();

    await rerender({ client, personID: 8 });
    await waitFor(() => expect(requests.some((request) => new URL(request.url).pathname === '/api/v1/people/8/agenda')).toBe(true));

    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(requests.filter((request) => request.method === 'POST').length).toBe(2));

    const posts = requests.filter((request) => request.method === 'POST');
    expect(new URL(posts[0].url).pathname).toBe('/api/v1/people/7/agenda');
    expect(new URL(posts[1].url).pathname).toBe('/api/v1/people/8/agenda');
    expect(posts[1].headers.get('Idempotency-Key')).not.toBe(posts[0].headers.get('Idempotency-Key'));
  });

  it('gates agenda mutations on a ready integration status', async () => {
    let ready = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) {
        return ready
          ? readyStatus()
          : Response.json({ state: 'authentication_required', message: 'Reconnect Kata to manage agenda items' });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('Kata integration authentication_required: Reconnect Kata to manage agenda items')).toBeDefined();
    // The gated agenda never requests the 503-prone list route, so no item
    // rows or Unlink controls render; the status message is the only notice.
    expect(screen.queryByRole('button', { name: 'Unlink Ask' })).toBeNull();
    expect(screen.queryByText('Task service is unavailable')).toBeNull();
    expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText('List') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(true);

    ready = true;
    await rerender({ client, personID: 8 });
    await waitFor(() => expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(false));
    expect(screen.queryByText('Kata integration authentication_required: Reconnect Kata to manage agenda items')).toBeNull();
    // The switch cleared person A's rows; person 8's list serves the same
    // item, so wait for it to render before asserting the Unlink gate.
    await screen.findByRole('button', { name: 'Unlink Ask' });
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    expect((screen.getByLabelText('List') as HTMLInputElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Unlink Ask' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('gates agenda mutations when the Kata API is incompatible', async () => {
    let compatible = false;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) {
        return compatible
          ? readyStatus()
          : Response.json({ state: 'incompatible', message: 'Kata API is incompatible.' });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('Kata integration incompatible: Kata API is incompatible.')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Unlink Ask' })).toBeNull();
    expect(screen.queryByText('Task service is unavailable')).toBeNull();
    expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText('List') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(true);

    compatible = true;
    await rerender({ client, personID: 8 });
    await waitFor(() => expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(false));
    expect(screen.queryByText('Kata integration incompatible: Kata API is incompatible.')).toBeNull();
    // The switch cleared person A's rows; person 8's list serves the same
    // item, so wait for it to render before asserting the Unlink gate.
    await screen.findByRole('button', { name: 'Unlink Ask' });
    expect((screen.getByLabelText('List') as HTMLInputElement).disabled).toBe(false);
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'Send notes' } });
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Unlink Ask' }) as HTMLButtonElement).disabled).toBe(false);
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
    expect(screen.queryByText('No linked Kata items.')).toBeNull();
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
    expect(screen.queryByText('No linked Kata items.')).toBeNull();
  });

  it('keeps the person page usable when Kata is unavailable', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      return Response.json({ error: 'task_integration_unavailable', message: 'Task service is unavailable' }, { status: 503 });
    }));
    render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('Task service is unavailable')).toBeDefined();
    expect(screen.getByRole('heading', { name: 'Agenda' })).toBeDefined();
  });

  it('distinguishes an available empty agenda from an unavailable one', async () => {
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      return Response.json({ project: 'msgvault', items: [] });
    }));
    render(PersonAgenda, { client, personID: 7 });
    expect(await screen.findByText('No linked Kata items.')).toBeDefined();
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
    expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText('List') as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(true);

    // A click that slips through must never pair A's ref with B's person id.
    await fireEvent.click(staleUnlink);
    expect(requests.some((request) => request.method === 'DELETE')).toBe(false);

    // Once B's status and list both resolve, the agenda recovers.
    releaseStatus!(readyStatus());
    await waitFor(() => expect(releaseList).not.toBeNull());
    releaseList!(Response.json({ project: 'msgvault', items: [] }));
    await screen.findByText('No linked Kata items.');
    await waitFor(() => expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(false));
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

  it('does not write a failed create for the prior person into the next person view', async () => {
    const onAnnounce = vi.fn();
    let releasePost: ((response: Response) => void) | null = null;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        return new Promise<Response>((resolve) => { releasePost = resolve; });
      }
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
      }
      return Response.json({ project: 'msgvault', items: [] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7, onAnnounce });
    await screen.findByText('No linked Kata items.');
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'A note' } });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(releasePost).not.toBeNull());

    await rerender({ client, personID: 8, onAnnounce });
    await screen.findByText('B item');
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'B note' } });

    // A's create fails after the switch: person B's view must not inherit
    // the failure, and the stale mutation must still release the mutation
    // mutex so B's form recovers.
    releasePost!(Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 }));
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByText('Unable to add agenda item')).toBeNull();
    expect((screen.getByLabelText('New agenda item') as HTMLInputElement).value).toBe('B note');
    expect(onAnnounce).not.toHaveBeenCalled();
  });

  it('does not apply a successful create from the prior person to the next person view', async () => {
    const onAnnounce = vi.fn();
    let releasePost: ((response: Response) => void) | null = null;
    let personEightLists = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'POST') {
        return new Promise<Response>((resolve) => { releasePost = resolve; });
      }
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        personEightLists += 1;
        return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
      }
      return Response.json({ project: 'msgvault', items: [] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7, onAnnounce });
    await screen.findByText('No linked Kata items.');
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'A note' } });
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(releasePost).not.toBeNull());

    await rerender({ client, personID: 8, onAnnounce });
    await screen.findByText('B item');
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'B note' } });

    // A's create succeeds after the switch: it must not clear B's typed
    // title, announce success, or trigger a redundant reload of B's list.
    releasePost!(Response.json({ item: item('new', 'A note') }, { status: 201 }));
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));
    expect((screen.getByLabelText('New agenda item') as HTMLInputElement).value).toBe('B note');
    expect(onAnnounce).not.toHaveBeenCalled();
    expect(personEightLists).toBe(1);
    expect(screen.getByText('B item')).toBeDefined();
  });

  it('does not write a failed unlink for the prior person into the next person view', async () => {
    let releaseDelete: ((response: Response) => void) | null = null;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'DELETE') {
        return new Promise<Response>((resolve) => { releaseDelete = resolve; });
      }
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask about launch')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7 });
    await fireEvent.click(await screen.findByRole('button', { name: 'Unlink Ask about launch' }));
    await waitFor(() => expect(releaseDelete).not.toBeNull());

    await rerender({ client, personID: 8 });
    await screen.findByText('B item');

    // A's unlink fails after the switch: B's view must not inherit the
    // error, and the stale mutation must release the mutation mutex.
    releaseDelete!(Response.json({ error: 'task_unavailable', message: 'Unable to unlink agenda item' }, { status: 503 }));
    await waitFor(() => expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(false));
    expect(screen.queryByText('Unable to unlink agenda item')).toBeNull();
    expect(screen.getByText('B item')).toBeDefined();
  });

  it('does not apply a successful unlink from the prior person to the next person view', async () => {
    const onAnnounce = vi.fn();
    let releaseDelete: ((response: Response) => void) | null = null;
    let personEightLists = 0;
    const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (isStatus(request)) return readyStatus();
      if (request.method === 'DELETE') {
        return new Promise<Response>((resolve) => { releaseDelete = resolve; });
      }
      if (new URL(request.url).pathname === '/api/v1/people/8/agenda') {
        personEightLists += 1;
        return Response.json({ project: 'msgvault', items: [item('two', 'B item')] });
      }
      return Response.json({ project: 'msgvault', items: [item('one', 'Ask about launch')] });
    }));

    const { rerender } = render(PersonAgenda, { client, personID: 7, onAnnounce });
    await fireEvent.click(await screen.findByRole('button', { name: 'Unlink Ask about launch' }));
    await waitFor(() => expect(releaseDelete).not.toBeNull());

    await rerender({ client, personID: 8, onAnnounce });
    await screen.findByText('B item');

    // A's unlink succeeds after the switch: it must not announce success or
    // trigger a redundant reload of B's list.
    releaseDelete!(Response.json({ item: item('one', 'Ask about launch') }));
    await waitFor(() => expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(false));
    expect(onAnnounce).not.toHaveBeenCalled();
    expect(personEightLists).toBe(1);
    expect(screen.getByText('B item')).toBeDefined();
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
    await screen.findByText('No linked Kata items.');
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'A note' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(releases.length).toBe(1)); // A's create hangs.

    // Person B is selected while A's create still hangs: B's form must be
    // usable immediately, not locked behind a request that may never settle.
    await rerender({ client, personID: 8 });
    await screen.findByText('B item');
    await waitFor(() => expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(false));

    // B starts a create; A's hung create then fails. The stale completion
    // must leave B's in-flight mutation alone instead of unlocking the form
    // while B's own request is still pending.
    await fireEvent.input(screen.getByLabelText('New agenda item'), { target: { value: 'B note' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }));
    await waitFor(() => expect(releases.length).toBe(2));
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(true);

    releases[0]!(Response.json({ error: 'task_unavailable', message: 'Unable to add agenda item' }, { status: 503 }));
    await waitFor(() => expect(released).toBe(1));
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(true);

    releases[1]!(Response.json({ item: item('new', 'B note') }, { status: 201 }));
    // B's success clears the typed title, so the submit button stays disabled
    // until text is entered again; the inputs are the mutating-only signal.
    await waitFor(() => expect((screen.getByLabelText('New agenda item') as HTMLInputElement).disabled).toBe(false));
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
    await waitFor(() => expect(screen.getByText('No linked Kata items.')).toBeDefined());
    const request = requests.find((candidate) => candidate.method === 'DELETE');
    expect(new URL(request!.url).pathname).toBe('/api/v1/people/7/agenda/one');
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

it('reports that a bounded agenda has more open tasks', async () => {
  const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (isStatus(request)) return readyStatus();
    return Response.json({ project: 'msgvault', items: [item('one', 'Ask')], truncated: true });
  }));
  render(PersonAgenda, { client, personID: 7 });
  expect(await screen.findByText('More open tasks are linked to this person. View the full list in Kata.')).toBeDefined();
});
