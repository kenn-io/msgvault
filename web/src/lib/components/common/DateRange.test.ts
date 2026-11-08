import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import DateRange from './DateRange.svelte';

it('retains the end date while Current disables it', async () => {
  render(DateRange, { start: '2024', end: '2025-03' });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
  expect(end).toHaveProperty('disabled', true);
  expect(end).toHaveProperty('value', '2025-03');
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
  expect(end).toHaveProperty('disabled', false);
  expect(end).toHaveProperty('value', '2025-03');
});

it('validates partial dates and date order under their fields', async () => {
  render(DateRange, { start: '2024-03', end: '2024-02' });
  expect(screen.getByText('End date must be on or after start date.')).toBeDefined();
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-04-31' } });
  expect(screen.getByText('Use 2024, 2024-03, or 2024-03-15.')).toBeDefined();
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-04-30' } });
  expect(screen.queryByText('Use 2024, 2024-03, or 2024-03-15.')).toBeNull();
});

it('shows date hints and places Current under the end date', async () => {
  const hint = () => screen.queryByText('2024, 2024-03, or 2024-03-15');
  const initial = render(DateRange, { start: '', current: true });
  expect(hint()).not.toBeNull();
  const start = screen.getByRole('textbox', { name: 'Start date' });
  await fireEvent.input(start, { target: { value: '2024-03' } });
  await fireEvent.focusIn(start);
  expect(hint()).not.toBeNull();
  await fireEvent.focusOut(start);
  expect(hint()).toBeNull();
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
  // An empty End date shows the forms once it's being typed.
  expect(hint()).toBeNull();
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.focusIn(end);
  expect(hint()).not.toBeNull();
  await fireEvent.input(end, { target: { value: '2024-13' } });
  expect(hint()).toBeNull();
  initial.unmount();
  const first = render(DateRange, { start: '', end: '', showCurrent: false });
  const hints = () => screen.queryAllByText('2024, 2024-03, or 2024-03-15');
  // Two empty fields share one hint, under the field being typed.
  expect(hints()).toHaveLength(1);
  const secondStart = screen.getByRole('textbox', { name: 'Start date' });
  const secondEnd = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.focusIn(secondEnd);
  expect(hints()).toHaveLength(1);
  expect(secondEnd.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
  await fireEvent.focusOut(secondEnd);
  await fireEvent.input(secondStart, { target: { value: '2024-03' } });
  expect(screen.getByText('Mar 2024')).toBeDefined();
  expect(secondEnd.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
  await fireEvent.input(secondEnd, { target: { value: '2025' } });
  expect(screen.getByText('2025')).toBeDefined();
  expect(hints()).toHaveLength(0);
  first.unmount();

  const view = render(DateRange, { start: '2024-03', current: true });
  const endField = view.container.querySelectorAll('.field')[1]!;
  const parts = () => Array.from(endField.children, (child) => child.className.split(' ')[0]);
  // End holds its hint line, empty or not, so Current stays under it.
  expect(parts()).toEqual(['kit-form-field', 'note', 'current']);
  expect(endField.querySelector('.note')!.textContent).toBe('');
  expect(endField.querySelector('.current')!.textContent).toContain('Current');
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
  expect(parts()).toEqual(['kit-form-field', 'note', 'current']);
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2025' } });
  expect(parts()).toEqual(['kit-form-field', 'note', 'current']);
  expect(endField.querySelector('.note')!.textContent).toBe('2025');
  // An error takes the hint line's place.
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-13' } });
  expect(parts()).toEqual(['kit-form-field', 'current']);
});
