import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
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

it.each(['ArrowDown', 'ArrowUp'])('opens the closed Organization picker with %s and selects an employer', async key => {
  const { controller } = controllerWith(async () => Response.json({ organizations: [organization()], total: 1, limit: 50, offset: 0 }));
  controller.organizations = [organization()];
  const onClose = vi.fn();
  const view = render(EmploymentEditor, { controller, personID: 7, onClose });
  try {
    const picker = screen.getByRole('button', { name: 'Organization: Choose an organization' });
    expect(screen.queryByRole('listbox')).toBeNull();
    await fireEvent.keyDown(picker, { key });
    const option = await screen.findByRole('option', { name: 'Synthetic Org' });
    expect(option).toBeDefined();
    await fireEvent.keyDown(screen.getByRole('combobox', { name: 'Organization' }), { key: 'Enter' });
    expect(screen.queryByRole('listbox')).toBeNull();
    const selected = await screen.findByRole('button', { name: 'Organization: Synthetic Org' });
    expect(screen.getByRole('button', { name: 'Create employment' })).toHaveProperty('disabled', false);
    await fireEvent.keyDown(selected, { key });
    await screen.findByRole('listbox');
    await fireEvent.keyDown(screen.getByRole('combobox', { name: 'Organization' }), { key: 'Escape' });
    expect(screen.queryByRole('listbox')).toBeNull();
    expect(onClose).not.toHaveBeenCalled();
    await fireEvent.keyDown(screen.getByRole('button', { name: 'Organization: Synthetic Org' }), { key: 'Escape' });
    expect(onClose).toHaveBeenCalledOnce();
  } finally {
    view.unmount();
    controller.destroy();
  }
});
