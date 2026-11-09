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

it('renders a date error under its field', async () => {
  render(DateRange, { start: '2024-03', end: '2024-02' });
  const end = screen.getByRole('textbox', { name: 'End date' });
  expect(end.closest('.field')!.textContent).toContain('End date must be on or after start date.');
  await fireEvent.input(screen.getByRole('textbox', { name: 'End date' }), { target: { value: '2024-04-30' } });
  expect(screen.queryByText('End date must be on or after start date.')).toBeNull();
});

it('shows one shared date hint under the field being edited when Current is on', async () => {
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
});

it('shows one shared date hint when Current is hidden', async () => {
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
  expect(secondEnd.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
  await fireEvent.input(secondEnd, { target: { value: '2025' } });
  expect(hints()).toHaveLength(0);
  first.unmount();
});
