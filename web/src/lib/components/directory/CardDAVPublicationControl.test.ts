import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import CardDAVPublicationControl from './CardDAVPublicationControl.svelte';

const unsafe = {
  raw_vcard: 'BEGIN:VCARD\nFN:FORBIDDEN\nEND:VCARD',
  url: 'https://forbidden.example.test/dav',
  href: '/forbidden/contact.vcf',
  private_marker: 'forbidden-private-marker'
};

function state(kind: 'unpublished' | 'published' | 'pending' | 'conflict', overrides: Record<string, unknown> = {}) {
  return {
    person_id: 7,
    state: kind,
    desired: kind === 'published',
    address_book: { id: 5, name: 'Synthetic contacts', ...unsafe },
    ...unsafe,
    ...overrides
  };
}

describe('CardDAVPublicationControl', () => {
  it.each([
    {
      name: 'unpublished',
      response: state('unpublished', { desired: true }),
      label: 'Publish person to CardDAV',
      checked: false,
      disabled: false,
      copy: ['Not published', 'Desired publication: Published', 'Publication address book: Synthetic contacts.']
    },
    {
      name: 'published',
      response: state('published', { desired: false }),
      label: 'Remove person from CardDAV',
      checked: true,
      disabled: false,
      copy: ['Published', 'Desired publication: Unpublished', 'Publication address book: Synthetic contacts.']
    },
    {
      name: 'pending',
      response: state('pending', { desired: true, pending_operation: 'create' }),
      label: 'Publish person to CardDAV',
      checked: true,
      disabled: true,
      copy: ['Publication pending', 'Desired publication: Published', 'CardDAV publication is waiting to create this contact.']
    },
    {
      name: 'conflict',
      response: state('conflict', { desired: false, conflict_id: 41 }),
      label: 'Remove person from CardDAV',
      checked: false,
      disabled: true,
      copy: ['Publication conflict', 'Desired publication: Unpublished']
    }
  ])('renders the generated $name state without unsafe response fields', async ({ response, label, checked, disabled, copy }) => {
    render(CardDAVPublicationControl, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(response))),
      personID: 7
    });

    expect(await screen.findByRole('heading', { name: 'CardDAV publication' })).toBeDefined();
    expect(await screen.findByText(copy[0]!)).toBeDefined();
    for (const text of copy.slice(1)) expect(screen.getByText(text)).toBeDefined();
    const toggle = screen.getByRole('switch', { name: label }) as HTMLInputElement;
    expect(toggle.checked).toBe(checked);
    expect(toggle.disabled).toBe(disabled);
    expect(document.body.textContent).not.toMatch(/FORBIDDEN|forbidden/i);
    expect(document.body.innerHTML).not.toMatch(/forbidden\.example|forbidden\/contact/i);
  });

  it('offers only positive conflict and missing-book handoffs without inventing a target', async () => {
    const onOpenConflict = vi.fn();
    const conflictRender = render(CardDAVPublicationControl, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(state('conflict', {
        conflict_id: 41
      })))),
      personID: 7,
      onOpenConflict
    });
    await fireEvent.click(await screen.findByRole('button', { name: 'Review CardDAV conflict 41' }));
    expect(onOpenConflict).toHaveBeenCalledOnce();
    expect(onOpenConflict).toHaveBeenCalledWith(41);
    conflictRender.unmount();

    const onOpenSettings = vi.fn();
    render(CardDAVPublicationControl, {
      client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(state('unpublished', {
        address_book: undefined
      })))),
      personID: 7,
      onOpenSettings
    });
    expect(await screen.findByText('No publish address book is selected.')).toBeDefined();
    expect(screen.queryByRole('switch')).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Open CardDAV settings' }));
    expect(onOpenSettings).toHaveBeenCalledOnce();
  });

  it('keeps a non-CardDAV-unavailable server failure actionable with the existing retry', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      if (reads === 1) {
        return Response.json({ error: 'carddav_runtime_failed', message: unsafe.private_marker }, { status: 503 });
      }
      return Response.json(state('unpublished'));
    });
    render(CardDAVPublicationControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load CardDAV publication state.');
    expect(document.body.textContent).not.toContain(unsafe.private_marker);
    await fireEvent.click(screen.getByRole('button', { name: 'Retry CardDAV publication state' }));
    expect(await screen.findByRole('switch', { name: 'Publish person to CardDAV' })).toBeDefined();
    expect(reads).toBe(2);
  });

  it('keeps a transport read failure actionable with the existing retry', async () => {
    let reads = 0;
    const fetchFn = vi.fn<typeof fetch>(async () => {
      reads += 1;
      if (reads === 1) throw new TypeError('synthetic connection reset');
      return Response.json(state('unpublished'));
    });
    render(CardDAVPublicationControl, { client: createAPIClient(fetchFn), personID: 7 });

    expect((await screen.findByRole('alert')).textContent).toContain('Unable to load CardDAV publication state.');
    await fireEvent.click(screen.getByRole('button', { name: 'Retry CardDAV publication state' }));
    expect(await screen.findByRole('switch', { name: 'Publish person to CardDAV' })).toBeDefined();
    expect(reads).toBe(2);
  });

  it('announces a clean mutation once and preserves focus on its connected intent control', async () => {
    let reads = 0;
    const onAnnounce = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') return Response.json(state('published'));
      reads += 1;
      return Response.json(state('unpublished'));
    });
    render(CardDAVPublicationControl, {
      client: createAPIClient(fetchFn), personID: 7, onAnnounce
    });

    const toggle = await screen.findByRole('switch', { name: 'Publish person to CardDAV' });
    toggle.focus();
    await fireEvent.click(toggle);

    const updated = await screen.findByRole('switch', { name: 'Remove person from CardDAV' });
    await waitFor(() => expect(document.activeElement).toBe(updated));
    expect(onAnnounce).toHaveBeenCalledOnce();
    expect(onAnnounce).toHaveBeenCalledWith('Published this person to CardDAV in Synthetic contacts.');
    expect(reads).toBe(1);
  });

  it('uses a connected heading fallback and truthful status when clean success returns pending', async () => {
    const onAnnounce = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      if (request.method === 'POST') return Response.json(state('pending', {
        desired: true,
        pending_operation: 'create'
      }));
      return Response.json(state('unpublished'));
    });
    render(CardDAVPublicationControl, {
      client: createAPIClient(fetchFn), personID: 7, onAnnounce
    });

    const toggle = await screen.findByRole('switch', { name: 'Publish person to CardDAV' });
    toggle.focus();
    await fireEvent.click(toggle);

    const heading = await screen.findByRole('heading', { name: 'CardDAV publication' });
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(onAnnounce).toHaveBeenCalledOnce();
    expect(onAnnounce).toHaveBeenCalledWith('CardDAV publication change is pending.');
    const pendingToggle = screen.getByRole('switch', { name: 'Publish person to CardDAV' }) as HTMLInputElement;
    expect(pendingToggle.checked).toBe(true);
    expect(pendingToggle.disabled).toBe(true);
  });
});
