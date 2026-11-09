import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { expect, it, vi } from 'vitest';
import DetailSection from './DetailSection.svelte';

it('puts history after current rows and gives a custom header control precedence over Add', async () => {
  const onadd = vi.fn();
  const view = render(DetailSection, { title: 'Work', addLabel: 'Add work', onadd, children: createRawSnippet(() => ({ render: () => '<p>Current job</p>' })), subgroupTitle: 'From address books', subgroup: createRawSnippet(() => ({ render: () => '<p>Imported job</p>' })), historyLabel: 'Show 2 past jobs', history: createRawSnippet(() => ({ render: () => '<p>Past jobs</p>' })) });
  const add = screen.getByRole('button', { name: 'Add work' });
  expect(add.closest('header')).not.toBeNull();
  await fireEvent.click(add);
  expect(onadd).toHaveBeenCalledOnce();
  expect(view.container.querySelector('details')!.open).toBe(false);
  await fireEvent.click(screen.getByText('Show 2 past jobs'));
  expect(view.container.querySelector('details')!.open).toBe(true);
  expect(Array.from(view.container.querySelector('.section-body')!.children, child => child.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Current job', 'Show 2 past jobs Past jobs', 'From address booksImported job']);
  await view.rerender({ title: 'Network', addLabel: 'Add work', onadd: vi.fn(), control: createRawSnippet(() => ({ render: () => '<button type="button">2 steps</button>' })) });
  const header = screen.getByRole('heading', { name: 'Network' }).parentElement!;
  expect(header.querySelector('button')?.textContent).toBe('2 steps');
  expect(screen.queryByRole('button', { name: 'Add work' })).toBeNull();
});
