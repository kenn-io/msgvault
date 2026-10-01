import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient, type APIClient } from '../../api/client';
import { FactLedgerController } from '../../directory/fact-ledger-controller.svelte';
import { openTypeahead } from '../../../test/kit-ui';
import FactReviewPanel from './FactReviewPanel.svelte';

afterEach(() => cleanup());

function renderFactPanel(props: {
  client: APIClient;
  personID: number | null;
  onSelectFactPerson?: (personID: number) => void;
  onOpenPerson?: (personID: number) => void;
}) {
  const controller = new FactLedgerController(props.client);
  if (props.personID !== null) controller.personID = props.personID;
  return render(FactReviewPanel, {
    controller,
    client: props.client,
    personID: props.personID,
    onSelectFactPerson: props.onSelectFactPerson ?? (() => undefined),
    onOpenPerson: props.onOpenPerson
  });
}

describe('FactReviewPanel', () => {
  it('offers the person picker and remains network-silent without a person', () => {
    const fetchFn = vi.fn<typeof fetch>();
    renderFactPanel({ client: createAPIClient(fetchFn), personID: null });

    const panel = screen.getByRole('region', { name: 'Facts' });
    expect(within(panel).getByText('Choose a person to see the facts recorded about them.')).toBeDefined();
    expect(within(panel).getByRole('button', { name: /^Person/ })).toBeDefined();
    expect(within(panel).queryByRole('button', { name: 'Open Directory' })).toBeNull();
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('picks a person for Facts from the Directory search', async () => {
    const requests: URL[] = [];
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(input instanceof Request ? input.url : String(input), 'http://localhost');
      requests.push(url);
      if (url.pathname === '/api/v1/people/directory') {
        return Response.json({
          people: [
            { id: 12, display_name: 'Alex Example', categories: [], organizations: [], contact_state: 'active', revision: 1 }
          ]
        });
      }
      return Response.json({});
    });
    const onSelectFactPerson = vi.fn();
    renderFactPanel({ client: createAPIClient(fetchFn), personID: null, onSelectFactPerson });

    const input = await openTypeahead('Person');
    await fireEvent.input(input, { target: { value: 'Alex' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Alex Example' }));

    expect(onSelectFactPerson).toHaveBeenCalledWith(12);
    expect(requests[0]!.searchParams.get('q')).toBe('Alex');
    expect(requests[0]!.searchParams.get('limit')).toBe('20');
  });

  it('says when nobody matches', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ people: [] }));
    renderFactPanel({ client: createAPIClient(fetchFn), personID: null });

    await fireEvent.input(await openTypeahead('Person'), { target: { value: 'Nobody' } });

    expect(await screen.findByText('No matching people')).toBeDefined();
  });

  it('shows a search failure inside the picker', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ message: 'Directory is offline' }, { status: 500 }));
    renderFactPanel({ client: createAPIClient(fetchFn), personID: null });

    await fireEvent.input(await openTypeahead('Person'), { target: { value: 'Alex' } });

    expect(await screen.findByText('Directory is offline')).toBeDefined();
  });

  it('shows the selected person by name after a reload', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () =>
      Response.json({ id: 12, display_name: 'Alex Example', participant_ids: [] })
    );
    renderFactPanel({ client: createAPIClient(fetchFn), personID: 12 });

    expect(await screen.findByText('Alex Example', { selector: 'strong' })).toBeDefined();
    expect(screen.queryByText('Person ID 12')).toBeNull();
    const paths = fetchFn.mock.calls.map(([input]) => new URL(input instanceof Request ? input.url : String(input), 'http://localhost').pathname);
    expect(paths).toContain('/api/v1/people/12');
  });

  it('says so when the person name cannot be loaded', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ message: 'boom' }, { status: 500 }));
    renderFactPanel({ client: createAPIClient(fetchFn), personID: 12 });

    expect(await screen.findByText("Couldn't load this person's name.")).toBeDefined();
    expect(screen.getByText('Person 12', { selector: 'strong' })).toBeDefined();
    expect(screen.getByRole('button', { name: 'Person: Person 12' })).toBeDefined();
  });

  it('drops the name-load note after picking another person', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input), 'http://localhost').pathname;
      if (path === '/api/v1/people/directory') {
        return Response.json({
          people: [
            { id: 13, display_name: 'Sam Example', categories: [], organizations: [], contact_state: 'active', revision: 1 }
          ]
        });
      }
      if (path === '/api/v1/people/12') return Response.json({ message: 'boom' }, { status: 500 });
      return new Promise<Response>(() => undefined);
    });
    const view = renderFactPanel({ client: createAPIClient(fetchFn), personID: 12 });
    expect(await screen.findByText("Couldn't load this person's name.")).toBeDefined();

    await fireEvent.input(await openTypeahead('Person'), { target: { value: 'Sam' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Sam Example' }));
    await view.rerender({ personID: 13 });

    expect(screen.getByText('Sam Example', { selector: 'strong' })).toBeDefined();
    expect(screen.queryByText("Couldn't load this person's name.")).toBeNull();
  });

  it('does not show the previous name for a different person', async () => {
    const fetchFn = vi.fn<typeof fetch>(async (input) => {
      const path = new URL(input instanceof Request ? input.url : String(input), 'http://localhost').pathname;
      if (path === '/api/v1/people/12') return Response.json({ id: 12, display_name: 'Alex Example', participant_ids: [] });
      return new Promise<Response>(() => undefined);
    });
    const client = createAPIClient(fetchFn);
    const view = renderFactPanel({ client, personID: 12 });
    expect(await screen.findByText('Alex Example', { selector: 'strong' })).toBeDefined();

    await view.rerender({ personID: 13 });

    expect(screen.queryByText('Alex Example')).toBeNull();
    expect(screen.getByText('Person 13', { selector: 'strong' })).toBeDefined();
  });

  it('falls back to a generic label while the name is unknown', () => {
    const fetchFn = vi.fn<typeof fetch>(() => new Promise<Response>(() => undefined));
    renderFactPanel({ client: createAPIClient(fetchFn), personID: 42 });

    expect(screen.getByText('Person 42', { selector: 'strong' })).toBeDefined();
  });

  it('renders exact honest gates and selected durable-person navigation without decision controls', async () => {
    const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ id: 42, display_name: 'Sam Example', participant_ids: [] }));
    const onOpenPerson = vi.fn();
    renderFactPanel({ client: createAPIClient(fetchFn), personID: 42, onOpenPerson });

    expect(screen.getByText('Fact candidate decisions are unavailable until a generated candidate contract is installed.')).toBeDefined();
    expect(screen.getByText('A dated last-time-we-talked brief is unavailable until the server exposes a generated brief contract.')).toBeDefined();
    expect(screen.queryByRole('button', { name: /accept|reject|unsure|run/i })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Open person profile' }));
    expect(onOpenPerson).toHaveBeenCalledWith(42);
  });
});
