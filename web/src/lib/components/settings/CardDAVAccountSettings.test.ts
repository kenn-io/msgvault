import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { SettingState } from '../../settings/catalog';
import CardDAVAccountSettings from './CardDAVAccountSettings.svelte';

const settings: SettingState[] = [
  accountSetting('carddav.base_url', 'string', { value: { string: 'https://old.example.test/' } }),
  accountSetting('carddav.username', 'string', { value: { string: 'alice' } }),
  accountSetting('carddav.password', 'secret', { secret: { configured: true } }),
  accountSetting('carddav.enabled', 'boolean', { value: { boolean: false } }),
  accountSetting('carddav.schedule', 'string', { value: { string: '0 2 * * *' } })
];

function accountSetting(
  key: string,
  kind: SettingState['kind'],
  fields: Partial<Pick<SettingState, 'value' | 'secret'>>
): SettingState {
  return {
    key,
    kind,
    group: 'sources',
    label: key,
    description: `Test fixture for ${key}.`,
    restart_required: false,
    ...fields
  };
}

function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((settle) => { resolve = settle; });
  return { promise, resolve };
}

describe('CardDAVAccountSettings', () => {
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
    render(CardDAVAccountSettings, { client: createAPIClient(fetchFn), settings });
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

  it('locks the complete account tuple while save is pending and clears only its submitted password', async () => {
    const deferred = deferredResponse();
    const onSaved = vi.fn();
    let requestFacts: { baseURL: string; username: string; enabled: boolean; schedule: string; passwordMatched: boolean } | undefined;
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      expect(body).toEqual({
        base_url: 'https://save.example.test/', username: 'save-user', password: 'save-password',
        enabled: true, schedule: '0 6 * * *'
      });
      requestFacts = {
        baseURL: String(body.base_url),
        username: String(body.username),
        enabled: Boolean(body.enabled),
        schedule: String(body.schedule),
        passwordMatched: body.password === 'save-password'
      };
      return deferred.promise;
    };
    render(CardDAVAccountSettings, { client: createAPIClient(fetchFn), settings, onSaved });
    await fireEvent.input(screen.getByLabelText('Base URL'), { target: { value: 'https://save.example.test/' } });
    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'save-user' } });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'save-password' } });
    await fireEvent.click(screen.getByRole('switch', { name: 'Enabled' }));
    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 6 * * *' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(requestFacts).toBeDefined());

    expectAccountControlsLocked('Saving…');
    (screen.getByLabelText('Password') as HTMLInputElement).focus();
    expect(document.activeElement).not.toBe(screen.getByLabelText('Password'));
    expect(requestFacts).toEqual({
      baseURL: 'https://save.example.test/', username: 'save-user', enabled: true,
      schedule: '0 6 * * *', passwordMatched: true
    });
    expect(JSON.stringify(requestFacts)).not.toContain('save-password');

    deferred.resolve(Response.json({
      base_url: 'https://save.example.test/', username: 'save-user', enabled: true,
      schedule: '0 6 * * *', books: 3
    }));
    expect((await screen.findByRole('status')).textContent).toContain('CardDAV account saved');
    expect((screen.getByLabelText('Base URL') as HTMLInputElement).value).toBe('https://save.example.test/');
    expect((screen.getByLabelText('Username') as HTMLInputElement).value).toBe('save-user');
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('');
    expect(onSaved).toHaveBeenCalledOnce();
  });

  it('retains one in-memory password from successful Test through exact Save, then clears it', async () => {
    const requestFacts: Array<{ method: string; path: string; passwordMatched: boolean }> = [];
    const onSaved = vi.fn();
    const fetchFn: typeof fetch = async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const body = await request.clone().json();
      expect(body).toEqual({
        base_url: 'https://dav.example.test/', username: 'alice', password: 'synthetic-password', enabled: false, schedule: '0 2 * * *'
      });
      requestFacts.push({ method: request.method, path: new URL(request.url).pathname, passwordMatched: body.password === 'synthetic-password' });
      return Response.json({ base_url: body.base_url, username: body.username, enabled: body.enabled, schedule: body.schedule, books: 2 });
    };
    render(CardDAVAccountSettings, { client: createAPIClient(fetchFn), settings, onSaved });
    await fireEvent.input(screen.getByLabelText('Base URL'), { target: { value: 'https://dav.example.test/' } });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-password' } });

    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    expect((await screen.findByRole('status')).textContent).toContain('Connection successful');
    expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe('synthetic-password');

    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(requestFacts).toHaveLength(2));
    expect(requestFacts).toEqual([
      { method: 'POST', path: '/api/v1/carddav/account/test', passwordMatched: true },
      { method: 'PUT', path: '/api/v1/carddav/account', passwordMatched: true }
    ]);
    expect(JSON.stringify(requestFacts)).not.toContain('synthetic-password');
    await waitFor(() => expect((screen.getByLabelText('Password') as HTMLInputElement).value).toBe(''));
    expect(onSaved).toHaveBeenCalledOnce();
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
    const rendered = render(CardDAVAccountSettings, { client: createAPIClient(fetchFn), settings });
    await fireEvent.input(screen.getByLabelText('Password'), { target: { value: 'synthetic-password' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Test CardDAV connection' }));
    expect((await screen.findByRole('status')).textContent).toContain('Connection successful');

    await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'bob' } });
    expect(screen.queryByText(/Connection successful/)).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
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
    render(CardDAVAccountSettings, { client: createAPIClient(fetchFn), settings });

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
    const rendered = render(CardDAVAccountSettings, { client: createAPIClient(fetchFn), settings, onSaved });
    await fireEvent.click(screen.getByRole('button', { name: 'Save CardDAV account' }));
    await waitFor(() => expect(signal).toBeDefined());

    rendered.unmount();
    expect(signal?.aborted).toBe(true);
    deferred.resolve(Response.json({ base_url: 'https://old.example.test/', username: 'alice', enabled: false, books: 1 }));
    await Promise.resolve();
    expect(onSaved).not.toHaveBeenCalled();
  });
});

function expectAccountControlsLocked(activeLabel: 'Testing…' | 'Saving…'): void {
  for (const label of ['Base URL', 'Username', 'Password', 'Schedule']) {
    expect((screen.getByLabelText(label) as HTMLInputElement).disabled).toBe(true);
  }
  expect((screen.getByRole('switch', { name: 'Enabled' }) as HTMLInputElement).disabled).toBe(true);
  expect((screen.getByRole('button', { name: activeLabel }) as HTMLButtonElement).disabled).toBe(true);
  const inactiveLabel = activeLabel === 'Testing…' ? 'Save CardDAV account' : 'Test CardDAV connection';
  expect((screen.getByRole('button', { name: inactiveLabel }) as HTMLButtonElement).disabled).toBe(true);
}
