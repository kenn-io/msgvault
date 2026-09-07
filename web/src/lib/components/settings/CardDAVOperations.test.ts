import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { CardDAVController } from '../../carddav/controller.svelte';
import CardDAVOperations from './CardDAVOperations.svelte';

describe('CardDAVOperations', () => {
  it('opens normalized CardDAV operation history through its shell callback', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL((input instanceof Request ? input : new Request(input)).url).pathname;
      if (path.endsWith('/status')) return Response.json({ configured: false, available: false, credential_configured: false, enabled: false, scheduled: false, schedule: '' });
      if (path.endsWith('/books')) return Response.json({ books: [] });
      return Response.json({ runs: [] });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    const onOpenOperations = vi.fn();
    render(CardDAVOperations, { controller, onOpenOperations });

    await fireEvent.click(screen.getByRole('button', { name: 'View CardDAV operations' }));

    expect(onOpenOperations).toHaveBeenCalledOnce();
    controller.destroy();
  });

  it('renders independent status, roles, history and never exposes a book URL marker', async () => {
    const forbidden = 'forbidden-url-marker.example.test/private';
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json({
        configured: true, available: true, credential_configured: true, enabled: false,
        scheduled: false, schedule: '', latest: {
          id: 4, trigger: 'manual', full: false, state: 'partial', started_at: '2026-08-28T10:00:00Z',
          finished_at: '2026-08-28T10:01:00Z', books: 2, created: 1, updated: 3, removed: 1,
          error_code: 'sync_failed', error_message: 'Some contacts need attention.'
        }
      });
      if (path.endsWith('/books')) return Response.json({ books: [{
        id: 7, name: 'Personal', url: `https://${forbidden}`, subscribed: true,
        lookup_source: false, write_target: false, needs_full_reconcile: true
      }] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    const rendered = render(CardDAVOperations, { controller });

    expect(screen.getByText('Configured')).toBeDefined();
    expect(screen.getByText('Runtime available')).toBeDefined();
    expect(screen.getByText('Manual sync only')).toBeDefined();
    expect(screen.getByText('Partial')).toBeDefined();
    expect(screen.getByText('2 books · 1 created · 3 updated · 1 removed')).toBeDefined();
    expect(screen.getByText('Full reconciliation required')).toBeDefined();
    expect(screen.getByText('No CardDAV sync history yet.')).toBeDefined();
    expect(rendered.container.textContent).not.toContain(forbidden);
    expect(rendered.container.innerHTML).not.toContain(forbidden);
    expect((screen.getByRole('button', { name: 'Sync now' }) as HTMLButtonElement).disabled).toBe(false);
    controller.destroy();
  });

  it('enforces publish-implies-sync in the visible draft and applies the exact row intent', async () => {
    const requests: Request[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) return Response.json({ configured: true, available: true, credential_configured: true, enabled: true, scheduled: true, schedule: '0 2 * * *' });
      if (request.method === 'GET' && path.endsWith('/books')) return Response.json({ books: [{ id: 7, name: 'Personal', url: 'https://forbidden.example.test', subscribed: false, lookup_source: false, write_target: false, needs_full_reconcile: false }] });
      if (path.endsWith('/runs')) return Response.json({ runs: [] });
      if (request.method === 'PATCH') return Response.json({ id: 7, name: 'Personal', url: 'https://forbidden.example.test', subscribed: true, lookup_source: false, write_target: true, needs_full_reconcile: false });
      throw new Error(`Unexpected ${request.method} ${path}`);
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVOperations, { controller });

    await fireEvent.click(screen.getByRole('checkbox', { name: 'Publish here for Personal' }));
    expect(screen.getByText('Publishing here also enables contact sync for this book.')).toBeDefined();
    expect((screen.getByRole('checkbox', { name: 'Sync contacts for Personal' }) as HTMLInputElement).checked).toBe(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Apply roles for Personal' }));
    await waitFor(() => expect(requests.some((request) => request.method === 'PATCH')).toBe(true));
    const patch = requests.find((request) => request.method === 'PATCH')!;
    await expect(patch.clone().json()).resolves.toEqual({ subscribed: true, lookup_source: false, write_target: true });
    controller.destroy();
  });

  it('shows fixed repair copy and keeps sync disabled without a ready runtime', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL((input instanceof Request ? input : new Request(input)).url).pathname;
      if (path.endsWith('/status')) return Response.json({ configured: true, available: false, credential_configured: false, enabled: true, scheduled: false, schedule: '', repair_reason: 'credential_missing' });
      if (path.endsWith('/books')) return Response.json({ books: [] });
      return Response.json({ runs: [] });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVOperations, { controller });

    expect(screen.getByRole('alert').textContent).toContain('No CardDAV password is stored. Enter the password and save the account.');
    expect((screen.getByRole('button', { name: 'Sync now' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText('No discovered address books.')).toBeDefined();
    controller.destroy();
  });

  it('keeps retained books read-only until status confirms runtime readiness', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/status')) {
        return Response.json({ error: 'unavailable', message: 'unsafe status detail' }, { status: 503 });
      }
      if (path.endsWith('/books')) {
        return Response.json({ books: [{
          id: 8, name: 'Retained book', url: 'https://forbidden.example.test', subscribed: true,
          lookup_source: false, write_target: false, needs_full_reconcile: false
        }] });
      }
      return Response.json({ runs: [{
        id: 5, trigger: 'scheduled', full: false, state: 'succeeded',
        started_at: '2026-08-28T09:00:00Z', finished_at: '2026-08-28T09:01:00Z',
        books: 1, created: 1, updated: 0, removed: 0
      }] });
    });
    const controller = new CardDAVController(createAPIClient(fetchFn));
    await controller.load();
    render(CardDAVOperations, { controller });

    expect((screen.getByRole('button', { name: 'Retry CardDAV status' }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText('1 books · 1 created · 0 updated · 0 removed')).toBeDefined();
    for (const control of screen.getAllByRole('checkbox')) {
      expect((control as HTMLInputElement).disabled).toBe(true);
    }
    expect((screen.getByRole('button', { name: 'Apply roles for Retained book' }) as HTMLButtonElement).disabled).toBe(true);
    controller.destroy();
  });
});
