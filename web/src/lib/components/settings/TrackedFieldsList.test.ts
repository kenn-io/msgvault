import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import { createAPIClient } from '../../api/client';
import TrackedFieldsList from './TrackedFieldsList.svelte';

describe('Fields kept updated', () => {
  it('retries an initial field catalog network failure in place', async () => {
    let failing = true;
    render(TrackedFieldsList, { client: createAPIClient(async input => {
      if (new URL((input as Request).url).pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [] });
      if (failing) throw new TypeError('Failed to fetch');
      return Response.json({ targets: [] });
    }) });
    const error = await screen.findByText("Couldn't load the fields kept updated.");
    expect(error.closest('details')).toBeNull();
    failing = false;
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('No fields yet')).toBeDefined();
  });

  it('keeps sensitive-field failure and Retry inside the open disclosure', async () => {
    let sensitiveReads = 0;
    const queries: string[] = [];
    const client = createAPIClient(async input => {
      const url = new URL((input as Request).url);
      if (url.pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [{ slug: 'favorite_plant', label: 'Preferred greenery' }, { slug: 'private_note', label: 'Private note' }] });
      queries.push(url.search);
      const sensitive = url.searchParams.get('include_sensitive') === 'true';
      if (sensitive && ++sensitiveReads === 1) return Response.json({ message: 'Synthetic lookup unavailable' }, { status: 503 });
      return Response.json({ targets: [
        { kind: 'attribute', key: 'field:favorite_plant', slug: 'favorite_plant', description: 'A plant', sensitive: false },
        { kind: 'employment', key: 'system:employment', slug: 'employment', description: 'Current and historical employment', sensitive: false },
        ...(sensitive ? [{ kind: 'attribute', key: 'field:private_note', slug: 'private_note', description: 'A note', sensitive: true }] : []),
      ] });
    });
    render(TrackedFieldsList, { client });
    expect(await screen.findByText('Preferred greenery')).toBeDefined();
    expect(screen.getByText('A plant')).toBeDefined();
    expect(screen.getByText('Work · job title, organization, and dates')).toBeDefined();
    expect(screen.getByText('Current and historical employment')).toBeDefined();
    const disclosure = screen.getByText('Show sensitive fields');
    await fireEvent.click(disclosure);
    const error = await screen.findByText("Couldn't load sensitive fields.");
    expect(error.closest('details')?.open).toBe(true);
    expect(screen.getByText('Preferred greenery')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    const sensitive = await screen.findByText('Private note');
    expect(screen.getByText('A note').closest('details')?.open).toBe(true);
    expect(sensitive.closest('details')?.open).toBe(true);
    expect(queries).toEqual(['?include_sensitive=false', '?include_sensitive=true', '?include_sensitive=true']);
    await fireEvent.click(disclosure);
    await waitFor(() => expect(sensitive.closest('details')?.open).toBe(false));
  });

  it('keeps loaded fields and retries unavailable field names', async () => {
    let names = 0, targets = 0;
    const client = createAPIClient(async input => {
      const url = new URL((input as Request).url);
      if (url.pathname.endsWith('/attribute-definitions')) {
        if (++names === 1) return Response.json({ message: 'Synthetic names unavailable' }, { status: 503 });
        return Response.json({ definitions: [{ slug: 'rr_27', label: 'Preferred greenery' }] });
      }
      targets++;
      return Response.json({ targets: [{ kind: 'attribute', key: 'field:rr_27', slug: 'rr_27', description: 'A plant', sensitive: false }] });
    });
    render(TrackedFieldsList, { client });
    expect(await screen.findByText('A plant')).toBeDefined();
    await fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('Preferred greenery')).toBeDefined();
    expect(targets).toBe(1);
    expect(names).toBe(2);
  });

  it.each([false, true])('lists a custom Employment field beside built-in Work, sensitive=%s', async sensitive => {
    const client = createAPIClient(async input => {
      const url = new URL((input as Request).url);
      if (url.pathname.endsWith('/attribute-definitions')) return Response.json({ definitions: [{ slug: 'employment', label: 'Employment' }] });
      return Response.json({ targets: [
        { key: 'system:employment', kind: 'employment', slug: 'employment', description: 'Work', sensitive: false },
        ...(!sensitive || url.searchParams.get('include_sensitive') === 'true' ? [{ key: 'field:employment', kind: 'attribute', slug: 'employment', description: 'Custom employment', sensitive }] : []),
      ] });
    });
    render(TrackedFieldsList, { client });
    if (sensitive) await fireEvent.click(screen.getByText('Show sensitive fields'));
    expect(await screen.findByText('Employment')).toBeDefined();
    expect(screen.getByText('Employment').closest('details') !== null).toBe(sensitive);
  });
});
