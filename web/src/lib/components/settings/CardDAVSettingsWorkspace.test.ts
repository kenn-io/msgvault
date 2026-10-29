import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import { chooseSelectOption } from '../../../test/kit-ui';
import CardDAVSettingsWorkspace from './CardDAVSettingsWorkspace.svelte';

const status = (name: string) => ({
  configured: true,
  available: true,
  credential_configured: true,
  enabled: true,
  scheduled: false,
  schedule: '0 2 * * *',
  account: {
    base_url: `https://${name}.example.test/`,
    username: `${name}@example.com`
  }
});

describe('CardDAV connection selection', () => {
  it('keeps shared conflicts available when the selected connection is unavailable', async () => {
    const requests: URL[] = [];
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const url = new URL(request.url);
      requests.push(url);
      if (url.pathname.endsWith('/connections'))
        return Response.json({
          connections: ['default', 'work'].map((connection) => ({
            connection,
            status: { ...status(connection), available: connection === 'work' }
          }))
        });
      if (url.pathname.endsWith('/status')) return Response.json({ ...status('default'), available: false });
      if (url.pathname.endsWith('/runs')) return Response.json({ runs: [] });
      if (url.pathname.endsWith('/conflicts')) return Response.json({ conflicts: [] });
      throw new Error(`Unexpected ${request.method} ${url.pathname}`);
    });
    const rendered = render(CardDAVSettingsWorkspace, { client, settings: [] });
    await screen.findByText('Runtime unavailable');
    await screen.findByRole('heading', { name: 'Unresolved conflicts' });
    expect(requests.some((url) => url.pathname.endsWith('/conflicts'))).toBe(true);
    expect(requests.some((url) => url.pathname.endsWith('/books'))).toBe(false);
    rendered.unmount();
  });

  it('isolates passwords, drafts, requests and late results when switching connections', async () => {
    const requests: Request[] = [];
    let resolve!: (value: Response) => void;
    const late = new Promise<Response>((done) => {
      resolve = done;
    });
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const url = new URL(request.url);
      if (url.pathname.endsWith('/connections'))
        return Response.json({
          connections: ['default', 'work'].map((connection) => ({
            connection,
            provider: '',
            status: status(connection)
          }))
        });
      if (url.pathname.endsWith('/test')) return late;
      if (url.pathname.endsWith('/status'))
        return Response.json(status(url.searchParams.get('connection') ?? 'default'));
      if (url.pathname.endsWith('/books')) return Response.json({ books: [] });
      if (url.pathname.endsWith('/runs')) return Response.json({ runs: [] });
      if (url.pathname.endsWith('/conflicts')) return Response.json({ conflicts: [] });
      return Response.json({});
    });
    render(CardDAVSettingsWorkspace, { client, settings: [] });
    await waitFor(() => expect(screen.getByRole('combobox', { name: /^CardDAV connection/ })).toBeDefined());
    await waitFor(() =>
      expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('default@example.com')
    );
    await fireEvent.input(screen.getByLabelText('Password'), {
      target: { value: 'synthetic-default-password' }
    });
    await fireEvent.input(screen.getByLabelText('Base URL'), {
      target: { value: 'https://draft.example.test/' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    await waitFor(() => expect(requests.some((r) => new URL(r.url).pathname.endsWith('/test'))).toBe(true));
    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV connection/ }), 'work');
    await waitFor(() =>
      expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('work@example.com')
    );
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
    expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe('https://work.example.test/');
    resolve(
      Response.json({
        base_url: 'https://draft.example.test/',
        username: 'default@example.com',
        enabled: true,
        books: 9
      })
    );
    await waitFor(() => expect(screen.queryByText(/Found 9/)).toBeNull());
    const body = await requests
      .find((r) => new URL(r.url).pathname.endsWith('/test'))!
      .clone()
      .json();
    expect(body.connection).toBe('default');
    expect(body.password).toBe('synthetic-default-password');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV connection/ }), 'default');
    await waitFor(() =>
      expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe(
        'https://default.example.test/'
      )
    );
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
  });
  it('creates a named form without borrowing the default credential', async () => {
    const bodies: Record<string, unknown>[] = [];
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/connections'))
        return Response.json({
          connections: [{ connection: 'default', status: status('default') }]
        });
      if (request.method === 'PUT') {
        bodies.push(await request.json());
        return Response.json({
          base_url: 'https://new.example.test/',
          username: 'person@example.com',
          enabled: true,
          books: 1
        });
      }
      if (path.endsWith('/status')) return Response.json(status('default'));
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      return Response.json({ conflicts: [] });
    });
    render(CardDAVSettingsWorkspace, { client, settings: [] });
    await waitFor(() => expect(screen.getByRole('combobox', { name: /^CardDAV connection/ })).toBeDefined());
    await fireEvent.input(screen.getByLabelText('New connection name'), {
      target: { value: 'work' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Add connection' }));
    await waitFor(() => expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe(''));
    await fireEvent.input(screen.getByLabelText('Base URL'), {
      target: { value: 'https://new.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Username'), {
      target: { value: 'person@example.com' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    expect(bodies).toHaveLength(0);
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(true);
    await fireEvent.input(screen.getByLabelText('Password'), {
      target: { value: 'synthetic-work-password' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({
      connection: 'work',
      username: 'person@example.com',
      password: 'synthetic-work-password'
    });
  });
});

it('restores an orphaned connection with its retained name and account details', async () => {
  const requests: Request[] = [];
  let saved = false;
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const url = new URL(request.url);
    const workStatus = {
      ...status('work'),
      configured: saved,
      available: saved,
      credential_configured: saved
    };
    if (url.pathname.endsWith('/connections')) {
      return Response.json({ connections: [{
        connection: 'work', account_id: 17, orphaned: !saved, status: workStatus
      }] });
    }
    if (url.searchParams.get('connection') === 'work' && !saved) {
      return Response.json({ error: 'bad_request', message: 'Invalid connection selector' }, { status: 400 });
    }
    if (url.pathname.endsWith('/status')) {
      return Response.json(url.searchParams.get('connection') === 'work'
        ? workStatus
        : { ...status('default'), configured: false, available: false });
    }
    if (url.pathname.endsWith('/account') && request.method === 'PUT') {
      saved = true;
      return Response.json({
        base_url: 'https://work.example.test/', username: 'work@example.com',
        enabled: true, schedule: '0 2 * * *', books: 1
      });
    }
    if (url.pathname.endsWith('/runs')) return Response.json({ runs: [] });
    if (url.pathname.endsWith('/books')) return Response.json({ books: [] });
    if (url.pathname.endsWith('/conflicts')) return Response.json({ conflicts: [] });
    throw new Error(`Unexpected ${request.method} ${url.pathname}`);
  });
  render(CardDAVSettingsWorkspace, { client, settings: [] });
  await screen.findByText('Runtime unavailable');
  await chooseSelectOption(
    screen.getByRole('combobox', { name: /^CardDAV connection/ }),
    'work (configuration missing)'
  );
  expect(screen.getByText(/Configuration is missing for connection “work”/)).toBeDefined();
  expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe('https://work.example.test/');
  expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('work@example.com');
  expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(true);
  expect(screen.queryByRole('heading', { name: 'CardDAV status' })).toBeNull();
  expect(requests.filter(request => new URL(request.url).searchParams.get('connection') === 'work')).toEqual([]);
  expect(requests.some(request => new URL(request.url).pathname.endsWith('/books'))).toBe(false);
  expect(requests.some(request => new URL(request.url).pathname.endsWith('/conflicts'))).toBe(false);

  await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-work-password' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));

  await waitFor(() => expect(screen.queryByText(/Configuration is missing/)).toBeNull());
  await screen.findByText('Runtime available');
  await screen.findByRole('heading', { name: 'Sync history' });
  await waitFor(() => expect(requests
    .map(request => new URL(request.url))
    .filter(url => url.searchParams.get('connection') === 'work')
    .map(url => url.pathname).sort()
  ).toEqual(['/api/v1/carddav/books', '/api/v1/carddav/runs', '/api/v1/carddav/status']));
  const save = requests.find(request => request.method === 'PUT')!;
  await expect(save.clone().json()).resolves.toEqual({
    connection: 'work', base_url: 'https://work.example.test/', username: 'work@example.com',
    enabled: true, schedule: '0 2 * * *', password: 'synthetic-work-password'
  });
});
