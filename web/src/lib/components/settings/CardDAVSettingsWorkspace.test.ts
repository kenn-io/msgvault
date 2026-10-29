import type { AccountDraft } from '../../carddav/account-settings';
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
    const name = 'constructor';
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
          connections: ['default', name].map((connection) => ({
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
    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV connection/ }), name);
    await waitFor(() =>
      expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe(`${name}@example.com`)
    );
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
    expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe(`https://${name}.example.test/`);
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-named-password' } });
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
        'https://draft.example.test/'
      )
    );
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('synthetic-default-password');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV connection/ }), name);
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('synthetic-named-password');
  });

});



it('discards a new connection selection and returns to the saved default account', async () => {
  const client = createAPIClient(async input => {
    const path = new URL((input as Request).url).pathname;
    if (path.endsWith('/connections')) return Response.json({ connections: [{ connection: 'default', provider: '', status: status('default') }] });
    if (path.endsWith('/status')) return Response.json(status('default'));
    if (path.endsWith('/books')) return Response.json({ books: [] });
    if (path.endsWith('/runs')) return Response.json({ runs: [] });
    return Response.json({ conflicts: [] });
  });
  const { rerender } = render(CardDAVSettingsWorkspace, { client, settings: [] });
  await screen.findByDisplayValue('default@example.com');
  await fireEvent.input(screen.getByLabelText('New connection name'), { target: { value: 'work' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Add connection' }));
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'draft@example.com' } });
  await rerender({ client, settings: [], drafts: {}, resetRevision: 1 });
  await waitFor(() => expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('default@example.com'));
  await fireEvent.click(screen.getByRole('combobox', { name: /^CardDAV connection/ }));
  expect(screen.queryByRole('option', { name: 'work' })).toBeNull();
});

  it('creates a named form without borrowing the default credential', async () => {
    const client = createAPIClient(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/connections'))
        return Response.json({
          connections: [{ connection: 'default', status: status('default') }]
        });
      if (path.endsWith('/status')) return Response.json(status('default'));
      if (path.endsWith('/books')) return Response.json({ books: [] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      return Response.json({ conflicts: [] });
    });
    let drafts: Record<string, AccountDraft> = {};
    render(CardDAVSettingsWorkspace, { client, settings: [], get drafts() { return drafts; }, set drafts(value) { drafts = value; } });
    await waitFor(() => expect(screen.getByRole('combobox', { name: /^CardDAV connection/ })).toBeDefined());
    await fireEvent.input(screen.getByLabelText('New connection name'), {
      target: { value: 'work' }
    });
    await fireEvent.click(screen.getByRole('button', { name: 'Add connection' }));
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
    await waitFor(() => expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe(''));
    await fireEvent.input(screen.getByLabelText('Base URL'), {
      target: { value: 'https://new.example.test/' }
    });
    await fireEvent.input(screen.getByLabelText('Username'), {
      target: { value: 'person@example.com' }
    });

    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(true);
    await fireEvent.input(screen.getByLabelText('Password'), {
      target: { value: 'synthetic-work-password' }
    });

    await waitFor(() => expect(drafts.work).toMatchObject({
      values: { username: 'person@example.com' }, password: 'synthetic-work-password'
    }));
    expect(drafts.work?.error).toBeUndefined();
  });

it('restores an orphaned connection with its retained name and account details', async () => {
  const requests: Request[] = [];
  const client = createAPIClient(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    requests.push(request);
    const url = new URL(request.url);
    const workStatus = {
      ...status('work'),
      configured: false,
      available: false,
      credential_configured: false
    };
    if (url.pathname.endsWith('/connections')) {
      return Response.json({ connections: [{
        connection: 'work', account_id: 17, orphaned: true, status: workStatus
      }] });
    }
    if (url.searchParams.get('connection') === 'work') {
      return Response.json({ error: 'bad_request', message: 'Invalid connection selector' }, { status: 400 });
    }
    if (url.pathname.endsWith('/status')) {
      return Response.json({ ...status('default'), configured: false, available: false });
    }
    if (url.pathname.endsWith('/runs')) return Response.json({ runs: [] });
    if (url.pathname.endsWith('/books')) return Response.json({ books: [] });
    if (url.pathname.endsWith('/conflicts')) return Response.json({ conflicts: [] });
    throw new Error(`Unexpected ${request.method} ${url.pathname}`);
  });
  let drafts: Record<string, AccountDraft> = {};
  render(CardDAVSettingsWorkspace, { client, settings: [], get drafts() { return drafts; }, set drafts(value) { drafts = value; } });
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
  await waitFor(() => expect(drafts.work).toMatchObject({
    values: { baseURL: 'https://work.example.test/', username: 'work@example.com', enabled: true, schedule: '0 2 * * *' },
    password: 'synthetic-work-password'
  }));
  expect(drafts.work?.error).toBeUndefined();
});
