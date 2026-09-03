import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import type { components } from '../../api/generated/schema';
import type { DirectoryReadBundle } from '../../directory/models';
import { DirectoryProfileController } from '../../directory/profile-controller.svelte';
import StructuredProfileEditor from './StructuredProfileEditor.svelte';

type PersonContactPoint = components['schemas']['PersonContactPoint'];

const when = '2026-08-01T00:00:00Z';

function person(revision = 3) {
  return {
    id: 7, revision, display_name: 'Test User', participant_ids: [], vcard_uid: 'person-7',
    created_at: when, updated_at: when
  };
}

function profile(revision = 3) {
  return { person: person(revision), names: [], contact_points: [], addresses: [], dates: [], categories: [], media: [] };
}

function bundle(): DirectoryReadBundle {
  return {
    person: person(), structuredProfile: profile(),
    etags: { person: '"person-7-r3"', structuredProfile: '"person-7-r3"' }, errors: {}
  };
}

function requestHarness(response: () => Response = () => new Response(JSON.stringify(profile(4)), {
  headers: { 'Content-Type': 'application/json', ETag: '"person-7-r4"' }
})) {
  const requests: Request[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    requests.push(input instanceof Request ? input : new Request(input));
    return response();
  });
  const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());
  return { controller, requests };
}

describe('StructuredProfileEditor', () => {
  it('supersedes a contact point with generated user provenance and no fabricated actor', async () => {
    const { controller, requests } = requestHarness();
    const current = {
      person_id: 7, address_kind: 'email', original_value: 'old@example.test',
      normalized_value: 'old@example.test', normalization: 'email', normalization_version: 1,
      envelope: { id: 11, ordinal: 2, source: 'archive_observation', created_at: when, updated_at: when, vcard: {} }
    } satisfies PersonContactPoint;
    render(StructuredProfileEditor, { controller, section: 'contact_points', current });

    await fireEvent.input(screen.getByLabelText('Email'), { target: { value: 'alice@example.test' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save contact point' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      contact_points: {
        add: [{
          address_kind: 'email', original_value: 'alice@example.test',
          envelope: { source: 'user', ordinal: 2 }
        }],
        supersede: [11]
      }
    });
    const body = await requests[0]!.clone().json() as { contact_points: { add: Array<Record<string, unknown>> } };
    expect(body.contact_points.add[0]).not.toHaveProperty('actor');
    expect(body.contact_points.add[0]!.envelope).not.toHaveProperty('actor');
  });

  it.each([
    {
      section: 'names' as const,
      label: 'Formatted name', value: 'Alice Example', save: 'Save name',
      expected: { names: { add: [{ name_kind: 'formatted', original_value: 'Alice Example', formatted: 'Alice Example', is_derived: false, envelope: { source: 'user' } }], supersede: [] } }
    },
    {
      section: 'addresses' as const,
      label: 'Street address', value: '1 Test Street', save: 'Save address',
      expected: { addresses: { add: [{ address_kind: 'postal', original_value: '1 Test Street', street_address: '1 Test Street', envelope: { source: 'user' } }], supersede: [] } }
    },
    {
      section: 'dates' as const,
      label: 'Date', value: '2000-02-03', save: 'Save date',
      expected: { dates: { add: [{ date_kind: 'birthday', original_value: '2000-02-03', date_text: '2000-02-03', date: { year: 2000, month: 2, day: 3 }, envelope: { source: 'user' } }], supersede: [] } }
    },
    {
      section: 'categories' as const,
      label: 'Category', value: 'Friends', save: 'Save category',
      expected: { categories: { add: [{ original_value: 'Friends', envelope: { source: 'user' } }], supersede: [] } }
    },
    {
      section: 'media' as const,
      label: 'Media URI', value: 'https://example.test/avatar.png', save: 'Save media metadata',
      expected: { media: { add: [{ media_kind: 'photo', uri: 'https://example.test/avatar.png', original_value: 'https://example.test/avatar.png', envelope: { source: 'user' } }], supersede: [] } }
    }
  ])('builds an explicit generated $section add', async ({ section, label, value, save, expected }) => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section });

    await fireEvent.input(screen.getByLabelText(label), { target: { value } });
    await fireEvent.click(screen.getByRole('button', { name: save }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual(expected);
  });

  it('adds an address from non-street components without inventing a street or original value', async () => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section: 'addresses' });

    await fireEvent.input(screen.getByLabelText('Locality'), { target: { value: 'Exampleville' } });
    await fireEvent.input(screen.getByLabelText('Country'), { target: { value: 'Testland' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      addresses: { add: [{
        address_kind: 'postal', locality: 'Exampleville', country_name: 'Testland', envelope: { source: 'user' }
      }], supersede: [] }
    });
  });

  it('edits an address represented by free text and geo without inventing a street', async () => {
    const { controller, requests } = requestHarness();
    const current = {
      person_id: 7, address_kind: 'birth_place', original_value: 'Original imported place',
      free_text: 'Exampleville, Testland', geo_uri: 'geo:1,2',
      envelope: { id: 26, ordinal: 8, source: 'vcard_import', created_at: when, updated_at: when, vcard: {} }
    };
    render(StructuredProfileEditor, { controller, section: 'addresses', current });

    expect(screen.getByLabelText('Street address')).toHaveProperty('value', '');
    await fireEvent.input(screen.getByLabelText('Country'), { target: { value: 'Testland' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      addresses: { add: [{
        address_kind: 'birth_place', original_value: 'Original imported place', free_text: 'Exampleville, Testland',
        geo_uri: 'geo:1,2', country_name: 'Testland', envelope: { source: 'user', ordinal: 8 }
      }], supersede: [26] }
    });
  });

  it('rejects an address with no server-supported value component', async () => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section: 'addresses' });

    await fireEvent.click(screen.getByRole('button', { name: 'Save address' }));

    expect((await screen.findByRole('alert')).textContent).toContain('Enter at least one address component.');
    expect(requests).toHaveLength(0);
  });

  it.each([
    {
      section: 'names' as const, label: 'Formatted name', value: 'Dr. Alice Example', save: 'Save name',
      current: {
        person_id: 7, name_kind: 'structured', original_value: 'Alice Example', formatted: 'Alice Example', family_name: 'Example', given_name: 'Alice',
        additional_names: 'Beatrice', honorific_prefixes: 'Dr.', honorific_suffixes: 'PhD', secondary_surname: 'Sample', generation: 'Jr.',
        language: 'en', script: 'Latn', phonetic_system: 'ipa', phonetic_script: 'Latn', sort_as: 'Example, Alice', is_derived: false,
        envelope: { id: 21, ordinal: 3, source: 'vcard_import', created_at: when, updated_at: when, vcard: {} }
      },
      expected: { names: { add: [{
        name_kind: 'structured', original_value: 'Dr. Alice Example', formatted: 'Dr. Alice Example', family_name: 'Example', given_name: 'Alice',
        additional_names: 'Beatrice', honorific_prefixes: 'Dr.', honorific_suffixes: 'PhD', secondary_surname: 'Sample', generation: 'Jr.',
        language: 'en', script: 'Latn', phonetic_system: 'ipa', phonetic_script: 'Latn', sort_as: 'Example, Alice', is_derived: false,
        envelope: { source: 'user', ordinal: 3 }
      }], supersede: [21] } }
    },
    {
      section: 'contact_points' as const, label: 'Contact value', value: 'alice.updated', save: 'Save contact point',
      current: {
        person_id: 7, address_kind: 'username', original_value: 'alice', normalized_value: 'alice', normalization: 'casefold', normalization_version: 1,
        service_slug: 'slack', scope_kind: 'workspace', scope_value: 'T-SYNTHETIC', uri: 'https://example.test/alice',
        envelope: { id: 22, ordinal: 4, source: 'user', created_at: when, updated_at: when, vcard: {} }
      },
      expected: { contact_points: { add: [{
        address_kind: 'username', original_value: 'alice.updated', service_slug: 'slack', scope_kind: 'workspace', scope_value: 'T-SYNTHETIC',
        uri: 'https://example.test/alice', envelope: { source: 'user', ordinal: 4 }
      }], supersede: [22] } }
    },
    {
      section: 'addresses' as const, label: 'Street address', value: '2 Updated Street', save: 'Save address',
      current: {
        person_id: 7, address_kind: 'postal', original_value: 'original-vcard-value', post_office_box: 'PO 12', extended_address: 'Suite 3',
        street_address: '1 Test Street', locality: 'Testville', region: 'TS', postal_code: '12345', country_name: 'Testland',
        extended_components: 'Building A', free_text: 'Deliver after noon', label: 'Home', geo_uri: 'geo:1,2', timezone: 'Etc/UTC',
        country_code: 'TS', place_uri: 'https://example.test/place',
        envelope: { id: 23, ordinal: 5, source: 'vcard_import', created_at: when, updated_at: when, vcard: {} }
      },
      expected: { addresses: { add: [{
        address_kind: 'postal', original_value: '2 Updated Street', post_office_box: 'PO 12', extended_address: 'Suite 3', street_address: '2 Updated Street',
        locality: 'Testville', region: 'TS', postal_code: '12345', country_name: 'Testland', extended_components: 'Building A',
        free_text: 'Deliver after noon', label: 'Home', geo_uri: 'geo:1,2', timezone: 'Etc/UTC', country_code: 'TS', place_uri: 'https://example.test/place',
        envelope: { source: 'user', ordinal: 5 }
      }], supersede: [23] } }
    },
    {
      section: 'dates' as const, label: 'Date label', value: 'Recurring date', save: 'Save date',
      current: {
        person_id: 7, date_kind: 'custom', original_value: '--0412', date: { month: 4, day: 12 }, date_text: '--04-12', label: 'Old label', calendar_scale: 'gregorian',
        envelope: { id: 24, ordinal: 6, source: 'vcard_import', created_at: when, updated_at: when, vcard: {} }
      },
      expected: { dates: { add: [{
        date_kind: 'custom', original_value: '--04-12', date_text: '--04-12', date: { month: 4, day: 12 }, label: 'Recurring date', calendar_scale: 'gregorian',
        envelope: { source: 'user', ordinal: 6 }
      }], supersede: [24] } }
    },
    {
      section: 'media' as const, label: 'Media URI', value: 'https://example.test/new-avatar.png', save: 'Save media metadata',
      current: {
        person_id: 7, media_kind: 'photo', original_value: 'Profile portrait', uri: 'https://example.test/avatar.png', media_type: 'image/png',
        has_data: false, byte_size: 1234, content_hash: 'sha256:synthetic',
        envelope: { id: 25, ordinal: 7, source: 'vcard_import', created_at: when, updated_at: when, vcard: {} }
      },
      expected: { media: { add: [{
        media_kind: 'photo', original_value: 'Profile portrait', uri: 'https://example.test/new-avatar.png', media_type: 'image/png',
        envelope: { source: 'user', ordinal: 7 }
      }], supersede: [25] } }
    }
  ])('preserves untouched generated fields when replacing $section', async ({ section, label, value, save, current, expected }) => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section, current });

    await fireEvent.input(screen.getByLabelText(label), { target: { value } });
    await fireEvent.click(screen.getByRole('button', { name: save }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual(expected);
  });

  it.each([
    {
      section: 'names' as const, selector: 'Name kind', nextKind: 'nickname', save: 'Save name', family: 'names',
      current: { person_id: 7, name_kind: 'formatted', original_value: 'Alice', formatted: 'Alice', is_derived: false, envelope: { id: 31, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} } }
    },
    {
      section: 'contact_points' as const, selector: 'Contact kind', nextKind: 'phone', save: 'Save contact point', family: 'contact_points',
      current: { person_id: 7, address_kind: 'email', original_value: 'alice@example.test', normalized_value: 'alice@example.test', normalization: 'email', normalization_version: 1, envelope: { id: 32, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} } }
    },
    {
      section: 'addresses' as const, selector: 'Address kind', nextKind: 'birth_place', save: 'Save address', family: 'addresses',
      current: { person_id: 7, address_kind: 'postal', original_value: '1 Test Street', street_address: '1 Test Street', envelope: { id: 33, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} } }
    },
    {
      section: 'dates' as const, selector: 'Date kind', nextKind: 'anniversary', save: 'Save date', family: 'dates',
      current: { person_id: 7, date_kind: 'birthday', original_value: '2000', date: { year: 2000 }, date_text: '2000', envelope: { id: 34, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} } }
    },
    {
      section: 'media' as const, selector: 'Media kind', nextKind: 'logo', save: 'Save media metadata', family: 'media',
      current: { person_id: 7, media_kind: 'photo', original_value: 'https://example.test/photo.png', uri: 'https://example.test/photo.png', has_data: false, envelope: { id: 35, ordinal: 2, source: 'user', created_at: when, updated_at: when, vcard: {} } }
    }
  ])('omits the prior ordinal when replacing $section with a different discriminator', async ({ section, selector, nextKind, save, family, current }) => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section, current });

    await fireEvent.change(screen.getByLabelText(selector), { target: { value: nextKind } });
    await fireEvent.click(screen.getByRole('button', { name: save }));

    await waitFor(() => expect(requests).toHaveLength(1));
    const body = await requests[0]!.clone().json() as Record<string, { add: Array<{ envelope: Record<string, unknown> }>; supersede: number[] }>;
    expect(body[family]!.add[0]!.envelope).toEqual({ source: 'user' });
    expect(body[family]!.supersede).toEqual([current.envelope.id]);
  });

  it('captures the service scope required by provider contact points', async () => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section: 'contact_points' });

    await fireEvent.change(screen.getByLabelText('Contact kind'), { target: { value: 'username' } });
    await fireEvent.input(screen.getByLabelText('Contact value'), { target: { value: 'alice' } });
    await fireEvent.input(screen.getByLabelText('Service'), { target: { value: 'slack' } });
    await fireEvent.input(screen.getByLabelText('Service scope kind'), { target: { value: 'workspace' } });
    await fireEvent.input(screen.getByLabelText('Service scope value'), { target: { value: 'synthetic-team' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save contact point' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      contact_points: { add: [{
        address_kind: 'username', original_value: 'alice', service_slug: 'slack',
        scope_kind: 'workspace', scope_value: 'synthetic-team', envelope: { source: 'user' }
      }], supersede: [] }
    });
  });

  it('keeps partial dates typed instead of flattening them to display text', async () => {
    const { controller, requests } = requestHarness();
    render(StructuredProfileEditor, { controller, section: 'dates' });

    await fireEvent.input(screen.getByLabelText('Date'), { target: { value: '2000' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save date' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    await expect(requests[0]!.clone().json()).resolves.toEqual({
      dates: { add: [{
        date_kind: 'birthday', original_value: '2000', date_text: '2000',
        date: { year: 2000 }, envelope: { source: 'user' }
      }], supersede: [] }
    });
  });

  it.each([
    ['2000-02-03', { year: 2000, month: 2, day: 3 }],
    ['2000-02', { year: 2000, month: 2 }],
    ['2000', { year: 2000 }],
    ['--02-03', { month: 2, day: 3 }],
    ['--02', { month: 2 }],
    ['---03', { day: 3 }]
  ])('formats and parses the supported typed partial date %s', async (formatted, date) => {
    const { controller, requests } = requestHarness();
    const current = {
      person_id: 7, date_kind: 'birthday', original_value: 'server-original', date, date_text: 'server-display',
      envelope: { id: 41, ordinal: 2, source: 'vcard_import', created_at: when, updated_at: when, vcard: {} }
    };
    render(StructuredProfileEditor, { controller, section: 'dates', current });

    expect(screen.getByLabelText('Date')).toHaveProperty('value', formatted);
    await fireEvent.click(screen.getByRole('button', { name: 'Save date' }));

    await waitFor(() => expect(requests).toHaveLength(1));
    const body = await requests[0]!.clone().json() as { dates: { add: Array<Record<string, unknown>> } };
    expect(body.dates.add[0]).toMatchObject({ date, original_value: formatted, date_text: formatted });
  });

  it('retains the entered value and exposes reload after a revision conflict', async () => {
    const { controller } = requestHarness(() => Response.json({
      error: 'person_revision_conflict', message: 'changed elsewhere'
    }, { status: 409 }));
    render(StructuredProfileEditor, { controller, section: 'categories' });

    const input = screen.getByLabelText('Category') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Close friends' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save category' }));

    expect((await screen.findByRole('alert')).textContent).toContain('This person changed elsewhere. Reload and retry.');
    expect(input.value).toBe('Close friends');
    expect(controller.draft).toMatchObject({ kind: 'profile' });
    expect(screen.getByRole('button', { name: 'Reload profile' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Save category' })).toHaveProperty('disabled', true);
  });

  it('keeps Save disabled through a stale reload and restores it only after a fresh strong profile ETag', async () => {
    let profileReloads = 0;
    const onDone = vi.fn();
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const request = input instanceof Request ? input : new Request(input);
      const path = new URL(request.url).pathname;
      if (request.method === 'PATCH') return Response.json({ error: 'precondition_required', message: 'Reload this profile.' }, { status: 428 });
      if (path === '/api/v1/people/7') return new Response(JSON.stringify(person(4)), { headers: { ETag: '"person-7-r4"' } });
      if (path.endsWith('/profile')) {
        profileReloads += 1;
        return new Response(JSON.stringify(profile(4)), { headers: { ETag: profileReloads === 1 ? '"person-7-r3"' : '"person-7-r4"' } });
      }
      if (path.endsWith('/attributes')) return Response.json({ person_id: 7, attributes: [] });
      return Response.json({ definitions: [] });
    });
    const controller = new DirectoryProfileController(createAPIClient(fetchFn), 7, bundle());
    render(StructuredProfileEditor, { controller, section: 'categories', onDone });

    await fireEvent.input(screen.getByLabelText('Category'), { target: { value: 'Close friends' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save category' }));
    expect(await screen.findByText('Reload this profile.')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Save category' })).toHaveProperty('disabled', true);

    await fireEvent.click(screen.getByRole('button', { name: 'Reload profile' }));
    await waitFor(() => expect(profileReloads).toBe(1));
    expect(onDone).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Save category' })).toHaveProperty('disabled', true);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Reload profile' })).toHaveProperty('disabled', false));

    await fireEvent.click(screen.getByRole('button', { name: 'Reload profile' }));
    await waitFor(() => expect(onDone).toHaveBeenCalledOnce());
    expect(controller.structuredProfileETag).toBe('"person-7-r4"');
    await waitFor(() => expect(screen.getByRole('button', { name: 'Save category' })).toHaveProperty('disabled', false));
  });

  it('shows a domain 409 verbatim without requiring a revision reload', async () => {
    const { controller } = requestHarness(() => Response.json({
      error: 'duplicate_category', message: 'That category is already current.'
    }, { status: 409 }));
    render(StructuredProfileEditor, { controller, section: 'categories' });

    const input = screen.getByLabelText('Category') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'Friends' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save category' }));

    expect((await screen.findByRole('alert')).textContent).toContain('That category is already current.');
    expect(input.value).toBe('Friends');
    expect(controller.conflict).toMatchObject({ code: 'request_failed' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Save category' })).toHaveProperty('disabled', false));
  });

  it('refuses a direct unsafe edit of inline media whose bytes are not in the read contract', () => {
    const { controller } = requestHarness();
    const current = {
      person_id: 7, media_kind: 'photo', original_value: 'Inline portrait', has_data: true, media_type: 'image/png', byte_size: 128, content_hash: 'sha256:synthetic',
      envelope: { id: 60, ordinal: 0, source: 'user', created_at: when, updated_at: when, vcard: {} }
    };

    render(StructuredProfileEditor, { controller, section: 'media', current });

    expect(screen.getByRole('note').textContent).toContain('Inline media content cannot be replaced from metadata alone.');
    expect(screen.queryByLabelText('Media URI')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Save media metadata' })).toBeNull();
  });
});
