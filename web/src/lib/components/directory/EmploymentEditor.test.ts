import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { controllerWith, organization } from '../../directory/entity-fixtures.test-support';
import EmploymentEditor from './EmploymentEditor.svelte';

it('shows loading, empty, and failed organization searches and recovers on another search', async () => {
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>(resolve => { finish = resolve; });
  const { controller } = controllerWith(async input => {
    const query = new URL((input as Request).url).searchParams.get('q');
    if (!query) return pending;
    if (query === 'failed') return Response.json({ message: 'Organization search unavailable' }, { status: 500 });
    return Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 });
  });
  controller.organizations = [organization()];
  const view = render(EmploymentEditor, { controller, personID: 7 });
  try {
    await fireEvent.keyDown(screen.getByRole('button', { name: /^Organization:/ }), { key: 'ArrowDown' });
    expect(await screen.findByText('Loading…')).toBeDefined();
    finish(Response.json({ organizations: [], total: 0, limit: 50, offset: 0 }));
    expect(await screen.findByText('No matches')).toBeDefined();
    const search = screen.getByRole('combobox', { name: 'Organization' });
    await fireEvent.input(search, { target: { value: 'failed' } });
    expect(await screen.findByRole('alert')).toHaveProperty('textContent', 'Organization search unavailable');
    await fireEvent.input(search, { target: { value: 'Synthetic' } });
    expect(await screen.findByRole('option', { name: 'Synthetic Org' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
  } finally {
    finish(Response.json({ organizations: [], total: 0, limit: 50, offset: 0 }));
    view.unmount();
    controller.destroy();
  }
});
