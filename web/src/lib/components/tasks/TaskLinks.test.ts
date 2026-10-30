import { taskCreationClient, taskOutboundMetadata } from './fixtures.test-support';
import { tick } from 'svelte';
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import TaskLinks from './TaskLinks.svelte';

function expectOneInitialTaskRead(requests: Request[]): void {
  expect(requests.filter(request => request.url.endsWith('/integrations/tasks/status'))).toHaveLength(1);
  expect(requests.filter(request => request.method === 'GET' && request.url.endsWith('/messages/42/tasks'))).toHaveLength(1);
}

describe('TaskLinks', () => {
  it('completes creation without refreshing the closing add surface', async () => {
    const reads: Request[] = [];
    const oncomplete = vi.fn();
    render(TaskLinks, { client: taskCreationClient(reads), mode: 'add', messageId: 42, title: 'Follow up', oncomplete });
    await fireEvent.click(await screen.findByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(oncomplete).toHaveBeenCalledOnce());
    await tick();
    expectOneInitialTaskRead(reads);
  });
  it('preserves a failed create draft and retry identity across New and Existing', async () => {
    const creates: Request[] = [];
    const client = createAPIClient(async (input) => {
      const request = input as Request;
      if (request.method === 'POST') {
        creates.push(request.clone());
        return creates.length === 1 ? Response.json({ message: 'Response lost after create' }, { status: 503 }) : Response.json({ task: { id: 'task-1', title: 'Revised follow up' } }, { status: 201 });
      }
      return Response.json(request.url.endsWith('/status') ? { state: 'ready', project: 'example' } : { state: 'ready', complete: true, tasks: [], outbound_metadata: taskOutboundMetadata });
    });
    render(TaskLinks, { client, mode: 'add', messageId: 42, title: 'Follow up' });
    await fireEvent.input(await screen.findByRole('textbox', { name: 'Task title' }), { target: { value: 'Revised follow up' } });
    await fireEvent.input(screen.getByLabelText('Description'), { target: { value: 'Draft notes' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await screen.findByText('Response lost after create');
    await fireEvent.click(screen.getByRole('radio', { name: 'Existing' }));
    expect(screen.queryByRole('textbox', { name: 'Task title' })).toBeNull();
    await fireEvent.click(screen.getByRole('radio', { name: 'New' }));
    expect(screen.getByRole('textbox', { name: 'Task title' })).toHaveProperty('value', 'Revised follow up');
    expect(screen.getByLabelText('Description')).toHaveProperty('value', 'Draft notes');
    await fireEvent.click(screen.getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(creates).toHaveLength(2));
    expect(creates[1].headers.get('X-Request-Id')).toBe(creates[0].headers.get('X-Request-Id'));
    const initial = await creates[0].json();
    expect(await creates[1].json()).toEqual(initial);
    expect(initial.added_at).toBeTruthy();
  });

  it('shows loading, linked tasks, and honest partial authority', async () => {
    let resolve!: (response: Response) => void;
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.url.endsWith('/integrations/tasks/status')) return Promise.resolve(Response.json({ state: 'ready', project: 'project', message: 'Ready' }));
      return new Promise((done) => { resolve = done; });
    });
    render(TaskLinks, { client: createAPIClient(fetchFn), messageId: 42, title: 'Synthetic' });
    expect(screen.getByRole('status').textContent).toContain('Loading linked tasks');
    await waitFor(() => expect(resolve).toBeTypeOf('function'));
    resolve(Response.json({ state: 'partial', complete: false, last_scan: '2026-07-19T01:00:00Z', remote_revision: 'remote-1', reason: 'safety_limit', tasks: [{ id: 'task-1', title: 'Follow up', revision: 'r1' }] }));
    expect(await screen.findByText('Follow up')).toBeDefined();
    expect(screen.getByRole('alert').textContent).toContain('partial');
    expect(screen.getByRole('alert').textContent).toContain('2026');
  });

  it.each([
    ['authentication_required', 'Authentication is required'],
    ['incompatible', 'incompatible'],
    ['unavailable', 'unavailable'],
    ['stale', 'stale'],
    ['partial', 'partial'],
    ['disabled', 'disabled'],
    ['not_found', 'not found'],
    ['wrong_project', 'wrong project']
  ])('renders the %s state without claiming there are no links', async (state, copy) => {
    render(TaskLinks, { client: createAPIClient(vi.fn(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.url.endsWith('/integrations/tasks/status')
        ? Response.json({ state: 'ready', project: 'project', message: 'Ready' })
        : Response.json({ state, complete: false, reason: state, last_scan: '2026-07-19T01:00:00Z', tasks: [] });
    })), messageId: 42, title: 'Synthetic' });
    expect((await screen.findByRole('alert')).textContent?.toLowerCase()).toContain(copy.toLowerCase());
    expect(screen.queryByText('No linked tasks.')).toBeNull();
  });

  it('loads last-good cached links even when integration status is unavailable', async () => {
    render(TaskLinks, { client: createAPIClient(vi.fn(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.url.endsWith('/integrations/tasks/status')
        ? Response.json({ state: 'unavailable', project: 'project', message: 'Unavailable' })
        : Response.json({ state: 'unavailable', complete: false, reason: 'daemon_unavailable', tasks: [{ id: 'task-1', title: 'Cached task' }], outbound_metadata: {
          archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Synthetic', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z'
        } });
    })), messageId: 42, title: 'Synthetic' });
    expect(await screen.findByText('Cached task')).toBeDefined();
    expect(screen.getByRole('alert').textContent).toContain('unavailable');
  });

  it.each(['disabled', 'not_found', 'authentication_required', 'unreachable', 'incompatible', 'wrong_project'])(
    'keeps cached tasks visible but disables mutations when integration status is %s',
    async (integrationState) => {
      const requests: Request[] = [];
      const onsettings = vi.fn();
      render(TaskLinks, { client: createAPIClient(vi.fn(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        requests.push(request);
        return request.url.endsWith('/integrations/tasks/status')
          ? Response.json({ state: integrationState, project: 'project', message: `Integration reason: ${integrationState}` })
          : Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-1', title: 'Cached task' }], outbound_metadata: {} });
      })), messageId: 42, title: 'Synthetic', onsettings });

      expect(await screen.findByText('Cached task')).toBeDefined();
      const unlink = screen.getByRole('button', { name: 'Unlink Cached task' });
      expect((unlink as HTMLButtonElement).disabled).toBe(true);
      expect(screen.getByRole(integrationState === 'disabled' ? 'status' : 'alert').textContent).toContain(`Integration reason: ${integrationState}`);
      await fireEvent.click(unlink);
      expect(requests.some((request) => request.method === 'DELETE')).toBe(false);
      await fireEvent.click(screen.getByRole('button', { name: integrationState === 'disabled' ? 'Set up' : 'Open Settings' }));
      expect(onsettings).toHaveBeenCalledOnce();
    }
  );

  it('keeps create, link, and unlink available on a partial index', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.url.endsWith('/integrations/tasks/status')) return Response.json({ state: 'ready', project: 'project', message: 'Ready' });
      return Response.json({
        state: 'partial', complete: false, reason: 'safety_limit', last_scan: '2026-07-19T01:00:00Z',
        tasks: [{ id: 'task-1', title: 'Partial task' }], outbound_metadata: {}
      });
    });
    const client = createAPIClient(fetchFn);
    const view = render(TaskLinks, { client, messageId: 42, title: 'Synthetic' });

    expect(await screen.findByText('Partial task')).toBeDefined();
    expect(screen.getByRole('alert').textContent).toContain('partial');
    expect((screen.getByRole('button', { name: 'Unlink Partial task' }) as HTMLButtonElement).disabled).toBe(false);
    await view.rerender({ mode: 'add' });
    expect(await screen.findByRole('button', { name: 'Create task' })).toBeDefined();
    expect(screen.getByRole('radio', { name: 'Existing' })).toBeDefined();
  });

  it.each(['stale', 'unavailable', 'incompatible', 'disabled', 'not_found', 'wrong_project', 'authentication_required'])(
    'keeps mutations disabled when the index state is %s even with a ready integration',
    async (state) => {
      const view = render(TaskLinks, { client: createAPIClient(vi.fn(async (input) => {
        const request = input instanceof Request ? input : new Request(input);
        return request.url.endsWith('/integrations/tasks/status')
          ? Response.json({ state: 'ready', project: 'project', message: 'Ready' })
          : Response.json({ state, complete: false, reason: state, tasks: state === 'authentication_required' ? [] : [{ id: 'task-1', title: 'Cached task' }], outbound_metadata: {} });
      })), mode: 'list', messageId: 42, title: 'Synthetic' });

      if (state === 'authentication_required') {
        expect((await screen.findByRole('alert')).textContent).toContain('Authentication is required');
        expect(screen.queryByRole('button', { name: /^Unlink/ })).toBeNull();
      } else {
        expect(await screen.findByRole('button', { name: 'Unlink Cached task' })).toHaveProperty('disabled', true);
      }
      await view.rerender({ mode: 'add' });
      expect(screen.queryByRole('button', { name: 'Create task' })).toBeNull();
      expect(screen.queryByRole('radio', { name: 'Existing' })).toBeNull();
    }
  );

  it('explains unsupported disk persistence while keeping safe current-session mutations ready', async () => {
    render(TaskLinks, { client: createAPIClient(vi.fn(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      return request.url.endsWith('/integrations/tasks/status')
        ? Response.json({ state: 'ready', project: 'project', message: 'Ready' })
        : Response.json({
          state: 'ready', complete: true, reason: 'cache_persistence_unsupported',
          tasks: [{ id: 'task-1', title: 'Current-session task' }], outbound_metadata: {}
        });
    })), messageId: 42, title: 'Synthetic' });

    expect(await screen.findByText('Current-session task')).toBeDefined();
    expect(screen.getByRole('status').textContent).toContain('not persisted to disk');
    expect((screen.getByRole('button', { name: 'Unlink Current-session task' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('ignores a late response for the previously selected message', async () => {
    const responses = new Map<number, (response: Response) => void>();
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.url.endsWith('/integrations/tasks/status')) return Promise.resolve(Response.json({ state: 'ready', project: 'project', message: 'Ready' }));
      const id = Number(request.url.match(/messages\/(\d+)\/tasks/)?.[1]);
      return new Promise((resolve) => responses.set(id, resolve));
    });
    const view = render(TaskLinks, { client: createAPIClient(fetchFn), messageId: 42, title: 'Message A' });
    await waitFor(() => expect(responses.has(42)).toBe(true));
    await view.rerender({ client: createAPIClient(fetchFn), messageId: 43, title: 'Message B' });
    expect(screen.queryByText('Task A')).toBeNull();
    await waitFor(() => expect(responses.has(43)).toBe(true));
    responses.get(43)!(Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-b', title: 'Task B' }], outbound_metadata: {} }));
    expect(await screen.findByText('Task B')).toBeDefined();
    responses.get(42)!(Response.json({ state: 'ready', complete: true, tasks: [{ id: 'task-a', title: 'Task A' }], outbound_metadata: {} }));
    await Promise.resolve();
    expect(screen.queryByText('Task A')).toBeNull();
    expect(screen.getByText('Task B')).toBeDefined();
  });

  it('does not let an unlink settlement mutate a newly selected message', async () => {
    let resolveDelete!: (response: Response) => void;
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>((input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      if (request.url.endsWith('/integrations/tasks/status')) return Promise.resolve(Response.json({ state: 'ready', project: 'project', message: 'Ready' }));
      if (request.method === 'DELETE') return new Promise((resolve) => { resolveDelete = resolve; });
      const id = Number(request.url.match(/messages\/(\d+)\/tasks/)?.[1]);
      return Promise.resolve(Response.json({ state: 'ready', complete: true, tasks: [{ id: `task-${id}`, title: `Task ${id}` }], outbound_metadata: {} }));
    });
    const client = createAPIClient(fetchFn);
    const view = render(TaskLinks, { client, messageId: 42, title: 'Message A' });
    await fireEvent.click(await screen.findByRole('button', { name: 'Unlink Task 42' }));
    await waitFor(() => expect(resolveDelete).toBeTypeOf('function'));
    await view.rerender({ client, messageId: 43, title: 'Message B' });
    expect(screen.queryByText('Task 42')).toBeNull();
    expect(await screen.findByText('Task 43')).toBeDefined();
    resolveDelete(Response.json({ task: { id: 'task-42', project: 'project', title: 'Task 42', revision: 'r2' } }));
    await Promise.resolve();
    expect(screen.getByText('Task 43')).toBeDefined();
    expect(screen.queryByText('Task 42')).toBeNull();
    expect(requests.filter((request) => request.method === 'GET' && request.url.includes('/messages/42/tasks'))).toHaveLength(1);
  });

  it('searches the configured project and links a selected result through the typed route', async () => {
    const requests: Request[] = [];
    const oncomplete = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input); requests.push(request);
      if (request.url.endsWith('/integrations/tasks/status')) return Response.json({ state: 'ready', project: 'project', message: 'Ready' });
      if (request.url.includes('/integrations/tasks/search')) return Response.json({ tasks: [{ id: 'task-1', title: 'Synthetic result', revision: 'r1' }] });
      if (request.method === 'POST') return Response.json({ task: { id: 'task-1', project: 'project', title: 'Linked', revision: 'r1' } }, { status: 201 });
      return Response.json({ state: 'ready', complete: true, last_scan: '2026-07-19T01:00:00Z', tasks: [], outbound_metadata: {
        archive_uid: 'archive-a', message_id: 42, conversation_id: 7, source_type: 'gmail', source_identifier: 'archive@example.com', source_message_id: 'source-42', subject: 'Synthetic', from: 'sender@example.com', sent_at: '2026-07-18T12:00:00Z'
      } });
    });
    render(TaskLinks, { client: createAPIClient(fetchFn), mode: 'add', messageId: 42, title: 'Synthetic', oncomplete });
    await fireEvent.click(await screen.findByRole('radio', { name: 'Existing' }));
    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'Synthetic' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    await fireEvent.click(await screen.findByRole('button', { name: 'Link Synthetic result' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'POST')).toBe(true));
    const post = requests.find((request) => request.method === 'POST')!;
    await expect(post.clone().json()).resolves.toMatchObject({ task_id: 'task-1' });
    expect(post.headers.get('X-Request-Id')).toBeTruthy();
    const body = await post.clone().json() as { added_at?: string };
    expect(body.added_at).toMatch(/^\d{4}-\d{2}-\d{2}T/);
    await waitFor(() => expect(oncomplete).toHaveBeenCalledOnce());
    expectOneInitialTaskRead(requests);
  });

  it('reuses one request identity and timestamp when retrying the same existing-task link', async () => {
    const posts: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.url.endsWith('/integrations/tasks/status')) return Response.json({ state: 'ready', project: 'project', message: 'Ready' });
      if (request.url.includes('/integrations/tasks/search')) return Response.json({ tasks: [{ id: 'task-1', title: 'Synthetic result', revision: 'r1' }] });
      if (request.method === 'POST') {
        posts.push(request);
        if (posts.length === 1) return Response.json({ message: 'Unavailable' }, { status: 503 });
        return Response.json({ task: { id: 'task-1', project: 'project', title: 'Linked', revision: 'r2' } }, { status: 201 });
      }
      return Response.json({ state: 'ready', complete: true, tasks: [], outbound_metadata: {} });
    });
    render(TaskLinks, {
      client: createAPIClient(fetchFn), mode: 'add', messageId: 42, title: 'Synthetic',
    });

    await fireEvent.click(await screen.findByRole('radio', { name: 'Existing' }));
    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'Synthetic' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    const link = await screen.findByRole('button', { name: 'Link Synthetic result' });
    await fireEvent.click(link);
    await screen.findByRole('alert');
    await fireEvent.click(link);
    await waitFor(() => expect(posts).toHaveLength(2));

    expect(posts[0]!.headers.get('X-Request-Id')).toBe(posts[1]!.headers.get('X-Request-Id'));
    await expect(posts[0]!.clone().text()).resolves.toBe(await posts[1]!.clone().text());
  });
});
