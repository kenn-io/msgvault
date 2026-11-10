import { fireEvent, render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { expect, it, vi } from 'vitest';
import FieldPicker from './FieldPicker.svelte';

const options = [{ name: 'garden', label: 'Example Garden' }];

it('offers creation last and shows the created fields with a way back', async () => {
  const oncreate = vi.fn(), onexisting = vi.fn();
  const creation = createRawSnippet(() => ({ render: () => '<label>Name<input value="Example School"></label>' }));
  const view = render(FieldPicker, { placeholder: 'Organization', fallbackLabel: 'Search or create', options, value: '', onselect: vi.fn(), oncreate, onexisting, creation, createLabel: 'Create organization “{query}”', existingLabel: 'Choose an existing organization instead' });
  await fireEvent.click(screen.getByRole('button', { name: 'Organization: Search or create' }));
  expect(screen.queryByRole('option', { name: /^Create organization/ })).toBeNull();
  await fireEvent.input(screen.getByRole('combobox'), { target: { value: '  example garden  ' } });
  expect(screen.queryByRole('option', { name: /^Create organization/ })).toBeNull();
  await fireEvent.input(screen.getByRole('combobox'), { target: { value: 'Example School' } });
  const rows = screen.getAllByRole('option');
  expect(rows.at(-1)?.textContent?.trim()).toBe('Create organization “Example School”');
  await fireEvent.mouseDown(rows.at(-1)!);
  expect(oncreate).toHaveBeenCalledWith('Example School');
  await view.rerender({ creating: true });
  expect(screen.getByRole('textbox', { name: 'Name' })).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Choose an existing organization instead' }));
  expect(onexisting).toHaveBeenCalledOnce();
  await view.rerender({ disabled: true });
  expect(screen.getByRole('button', { name: 'Choose an existing organization instead' })).toHaveProperty('disabled', true);
});

it('keeps Retry inside the failed lookup and sends queries to its loader', async () => {
  const onretry = vi.fn(), onquery = vi.fn();
  render(FieldPicker, { placeholder: 'Person', options: [], value: '', onselect: vi.fn(), error: "Couldn't search people.", onretry, onquery });
  await fireEvent.click(screen.getByRole('button', { name: 'Person' }));
  expect(screen.getByRole('alert').textContent).toContain("Couldn't search people.");
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(onretry).toHaveBeenCalledOnce();
  await fireEvent.input(screen.getByRole('combobox'), { target: { value: 'garden' } });
  expect(onquery).toHaveBeenLastCalledWith('garden');
});
