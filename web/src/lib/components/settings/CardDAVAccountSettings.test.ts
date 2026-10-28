import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { chooseSelectOption } from '../../../test/kit-ui';
import { createAPIClient } from '../../api/client';
import type { CardDAVAccountValues } from '../../carddav/account-settings';
import CardDAVAccountSettings from './CardDAVAccountSettings.svelte';

const values: CardDAVAccountValues = {
  provider: '',
  oauthApp: '',
  baseURL: 'https://old.example.test/',
  username: 'alice',
  passwordConfigured: true,
  enabled: false,
  schedule: '0 2 * * *'
};

const refreshedValues: CardDAVAccountValues = {
  provider: '',
  oauthApp: '',
  baseURL: 'https://fresh.example.test/',
  username: 'bob',
  passwordConfigured: false,
  enabled: true,
  schedule: '0 4 * * *'
};

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

describe('CardDAVAccountSettings', () => {
  it.each(['https://old.example.test/', 'https://draft.example.test/'])('preserves the CardDAV URL %s when switching providers without saving', async (baseURL) => {
    render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(async () => Response.json({})), values });
    await fireEvent.input(screen.getByLabelText('Base URL'), { target: { value: baseURL } });

    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV provider/ }), 'Google Contacts');
    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV provider/ }), 'Other CardDAV server');

    expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe(baseURL);
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(baseURL !== 'https://old.example.test/');
  });

  it('shows a sign-in command that does not save connection', async () => {
    render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(async () => Response.json({})), values, connection: 'work' });

    await chooseSelectOption(screen.getByRole('combobox', { name: /^CardDAV provider/ }), 'Microsoft 365 or Outlook.com');

    expect(screen.queryByLabelText('Base URL')).toBeNull();
    expect(screen.queryByLabelText('Password')).toBeNull();
  });

  it.each([
    ['google', `msgvault carddav authorize-google 'bob@example.com' --oauth-app 'Work App'`, 'msgvault carddav authorize-google "bob@example.com" --oauth-app "Work App"'],
    ['microsoft', `msgvault carddav authorize-microsoft 'bob@example.com'`, 'msgvault carddav authorize-microsoft "bob@example.com"'],
  ])('shows labeled commands for both shells for %s', (provider, posixCommand, cmdCommand) => {
    const oauthValues = { ...values, provider, oauthApp: 'Work App', username: 'bob@example.com' };
    render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(async () => Response.json({})), values: oauthValues });

    expect(screen.getByText('POSIX shell')).toBeDefined();
    expect(screen.getByText('Command Prompt')).toBeDefined();
    expect(screen.getByText(posixCommand)).toBeDefined();
    expect(screen.getByText(cmdCommand)).toBeDefined();
    expect(screen.getByText(/Start Command Prompt with/).textContent).toBe('Start Command Prompt with cmd /d /v:off, then paste:');
  });

  it('refreshes a clean account form when settings props change', async () => {
    const client = createAPIClient(async () => Response.json({}));
    const rendered = render(CardDAVAccountSettings, { onDraft: vi.fn(), client, values });

    await rendered.rerender({ client, values: refreshedValues });

    await waitFor(() => expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe('https://fresh.example.test/'));
    expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('bob');
    expect((screen.getByRole('switch', { name: 'Enabled' }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText('Schedule') as HTMLInputElement).value).toBe('0 4 * * *');
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(true);
  });

  it('preserves an intentional local edit while refreshing untouched fields', async () => {
    const client = createAPIClient(async () => Response.json({}));
    const rendered = render(CardDAVAccountSettings, { onDraft: vi.fn(), client, values });
    await fireEvent.input(screen.getByLabelText('Base URL'), {
      target: { value: 'https://draft.example.test/' }
    });

    await rendered.rerender({ client, values: refreshedValues });

    await waitFor(() => expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('bob'));
    expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe('https://draft.example.test/');
  });

  it('stages a credential-free disable for a saved account', async () => {
    const onDraft = vi.fn();
    render(CardDAVAccountSettings, { client: createAPIClient(vi.fn()), values: refreshedValues, onDraft });
    await fireEvent.click(screen.getByRole('switch', { name: 'Enabled' }));
    await waitFor(() => expect(onDraft).toHaveBeenLastCalledWith(expect.objectContaining({
      request: { base_url: 'https://fresh.example.test/', username: 'bob', enabled: false, schedule: '0 4 * * *' }, error: undefined
    })));
    expect((screen.getByLabelText('Password') as HTMLInputElement).required).toBe(false);
  });

  it('locks the complete account tuple while a connection test is pending', async () => {
    const deferred = deferredResponse();
    let requestFacts: { baseURL: string; username: string; enabled: boolean; schedule: string; passwordMatched: boolean } | undefined;
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      expect(body).toEqual({
        base_url: 'https://test.example.test/', username: 'test-user', password: 'test-password',
        enabled: true, schedule: '0 5 * * *'
      });
      requestFacts = {
        baseURL: String(body.base_url),
        username: String(body.username),
        enabled: Boolean(body.enabled),
        schedule: String(body.schedule),
        passwordMatched: body.password === 'test-password'
      };
      return deferred.promise;
    };
    render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(fetchFn), values });
    await fireEvent.input(screen.getByLabelText('Base URL'), { target: { value: 'https://test.example.test/' } });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'test-user' } });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'test-password' } });
    await fireEvent.click(screen.getByRole('switch', { name: 'Enabled' }));
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 5 * * *' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    await waitFor(() => expect(requestFacts).toBeDefined());

    expectAccountControlsLocked('Testing…');
    (screen.getByLabelText('Password') as HTMLInputElement).focus();
    expect(document.activeElement).not.toBe(screen.getByLabelText('Password'));
    expect(requestFacts).toEqual({
      baseURL: 'https://test.example.test/', username: 'test-user', enabled: true,
      schedule: '0 5 * * *', passwordMatched: true
    });
    expect(JSON.stringify(requestFacts)).not.toContain('test-password');

    deferred.resolve(Response.json({
      base_url: 'https://test.example.test/', username: 'test-user', enabled: true,
      schedule: '0 5 * * *', books: 2
    }));
    expect((await screen.findByRole('status')).textContent).toContain('Connection successful');
    expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe('https://test.example.test/');
    expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('test-user');
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('test-password');
  });



  it('retains password after failure and invalidates tested success when identity changes', async () => {
    let requests = 0;
    const requestFacts: Array<{ passwordMatched: boolean; username: string }> = [];
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      requests += 1;
      expect(body).toEqual({
        base_url: 'https://old.example.test/', username: requests === 1 ? 'alice' : 'bob',
        password: 'synthetic-password', enabled: false, schedule: '0 2 * * *'
      });
      requestFacts.push({ passwordMatched: body.password === 'synthetic-password', username: String(body.username) });
      if (requests === 1) return Response.json({ base_url: 'https://old.example.test/', username: 'alice', enabled: false, books: 1 });
      return Response.json({ error: 'carddav_unavailable', message: 'CardDAV server refused the account (synthetic reason).' }, { status: 503 });
    };
    const rendered = render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(fetchFn), values });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-password' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    expect((await screen.findByRole('status')).textContent).toContain('Connection successful');

    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'bob' } });
    expect(screen.queryByText(/Connection successful/)).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    expect((await screen.findByRole('alert')).textContent).toBe('CardDAV server refused the account (synthetic reason).');
    expect(rendered.container.textContent).not.toContain('synthetic-password');
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('synthetic-password');
    expect(requestFacts).toEqual([
      { passwordMatched: true, username: 'alice' },
      { passwordMatched: true, username: 'bob' }
    ]);
    expect(JSON.stringify(requestFacts)).not.toContain('synthetic-password');
  });

  it('shows the server message when a connection test fails and falls back for a network failure', async () => {
    let requests = 0;
    const fetchFn: typeof fetch = async () => {
      requests += 1;
      if (requests === 1) {
        return Response.json({ error: 'carddav_unreachable', message: 'CardDAV host did not answer (synthetic reason).' }, { status: 502 });
      }
      throw new TypeError('network down');
    };
    render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(fetchFn), values });

    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    expect((await screen.findByRole('alert')).textContent).toBe('CardDAV host did not answer (synthetic reason).');

    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    await waitFor(() => expect(screen.getByRole('alert').textContent).toBe('network down'));
  });

  it('aborts a deferred account request and never refreshes after destruction', async () => {
    const deferred = deferredResponse();
    let signal: AbortSignal | undefined;
    const onSaved = vi.fn();
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      expect(body).toEqual({
        base_url: 'https://old.example.test/', username: 'alice', enabled: false, schedule: '0 2 * * *'
      });
      signal = request.signal;
      return deferred.promise;
    };
    const rendered = render(CardDAVAccountSettings, { onDraft: vi.fn(), client: createAPIClient(fetchFn), values });
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    await waitFor(() => expect(signal).toBeDefined());

    rendered.unmount();
    expect(signal?.aborted).toBe(true);
    deferred.resolve(Response.json({ base_url: 'https://old.example.test/', username: 'alice', enabled: false, books: 1 }));
    await Promise.resolve();

  });
});

function expectAccountControlsLocked(activeLabel: 'Testing…' | 'Saving…'): void {
  for (const label of ['Base URL', 'Username', 'Password', 'Schedule']) {
    expect((screen.getByLabelText(label) as HTMLInputElement).disabled).toBe(true);
  }
  expect((screen.getByRole('switch', { name: 'Enabled' }) as HTMLInputElement).disabled).toBe(true);
  expect((screen.getByRole('button', { name: activeLabel }) as HTMLButtonElement).disabled).toBe(true);

}
