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

it('shows the format hint only while a date is empty or being typed', async () => {
  const hint = () => screen.queryByText('2024, 2024-03, or 2024-03-15');
  render(DateRange, { start: '', current: true });
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
});

it('reads a filled date back in words where its hint was', async () => {
  render(DateRange, { start: '', end: '', showCurrent: false });
  const hints = () => screen.queryAllByText('2024, 2024-03, or 2024-03-15');
  // Two empty fields share one hint, under the field being typed.
  expect(hints()).toHaveLength(1);
  const start = screen.getByRole('textbox', { name: 'Start date' });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.focusIn(end);
  expect(hints()).toHaveLength(1);
  expect(end.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
  await fireEvent.focusOut(end);
  await fireEvent.input(start, { target: { value: '2024-03' } });
  expect(screen.getByText('Mar 2024')).toBeDefined();
  expect(end.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
  await fireEvent.input(end, { target: { value: '2025' } });
  expect(screen.getByText('2025')).toBeDefined();
  expect(hints()).toHaveLength(0);
});

it("puts Current in End's hint line while End has nothing to read back, and under its reading otherwise", async () => {
  const view = render(DateRange, { start: '2024-03', current: true });
  const end = view.container.querySelectorAll('.field')[1]!;
  const parts = () => Array.from(end.children, (child) => child.className.split(' ')[0]);
  expect(parts()).toEqual(['kit-form-field', 'current']);
  expect(end.querySelector('.current')!.textContent).toContain('Current');
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
  expect(parts()).toEqual(['kit-form-field', 'current']);
  // Focusing Current isn't typing End, so Current stays where it was clicked.
  await fireEvent.focusIn(screen.getByRole('checkbox', { name: 'Current' }));
  expect(parts()).toEqual(['kit-form-field', 'current']);
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2025' } });
  expect(parts()).toEqual(['kit-form-field', 'note', 'current']);
  expect(end.querySelector('.note')!.textContent).toBe('2025');
  expect(end.querySelector('.current')!.classList.contains('below')).toBe(true);
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-13' } });
  expect(parts()).toEqual(['kit-form-field', 'current']);
  expect(end.querySelector('.current')!.classList.contains('below')).toBe(true);
});
