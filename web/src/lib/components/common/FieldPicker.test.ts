import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import FieldPicker from './FieldPicker.svelte';

afterEach(cleanup);
const options = [{ name: 'garden', label: 'Example Garden' }];

it('offers creation last and shows the created fields with a way back', async () => {
  const oncreate = vi.fn(), onexisting = vi.fn();
  const creation = createRawSnippet(() => ({ render: () => '<label>Name<input value="Example School"></label>' }));
  const view = render(FieldPicker, { placeholder: 'Organization', fallbackLabel: 'Search or create', options, value: '', onselect: vi.fn(), oncreate, onexisting, creation, createLabel: 'Create organization “{query}”', existingLabel: 'Choose an existing organization instead' });
  await fireEvent.click(screen.getByRole('button', { name: 'Organization: Search or create' }));
  // The open search says what the closed picker did, under the field's own name.
  await waitFor(() => expect(screen.getByRole('combobox', { name: 'Organization' })).toHaveProperty('placeholder', 'Search or create'));
  await fireEvent.input(screen.getByRole('combobox'), { target: { value: 'Example School' } });
  const rows = screen.getAllByRole('option');
  expect(rows.at(-1)?.textContent?.trim()).toBe('Create organization “Example School”');
  await fireEvent.mouseDown(rows.at(-1)!);
  expect(oncreate).toHaveBeenCalledWith('Example School');
  await view.rerender({ creating: true });
  expect(screen.getByRole('textbox', { name: 'Name' })).toHaveProperty('value', 'Example School');
  await fireEvent.click(screen.getByRole('button', { name: 'Choose an existing organization instead' }));
  expect(onexisting).toHaveBeenCalledOnce();
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

it('offers creation when the query matches an organization ID', async () => {
  const oncreate = vi.fn();
  render(FieldPicker, { placeholder: 'Organization', options: [{ name: '21', label: '21 Labs' }], value: '', onselect: vi.fn(), oncreate, createLabel: 'Create organization “{query}”' });
  await fireEvent.click(screen.getByRole('button', { name: 'Organization' }));
  await fireEvent.input(screen.getByRole('combobox'), { target: { value: '21' } });
  await fireEvent.mouseDown(screen.getByRole('option', { name: 'Create organization “21”' }));
  expect(oncreate).toHaveBeenCalledWith('21');
});
