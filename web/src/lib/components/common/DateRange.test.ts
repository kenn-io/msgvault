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
});

it.each([
  ['Start date', false], ['End date', false], ['End date', true],
] as const)('shows the format instructions once when %s is invalid, Current shown=%s', async (label, showCurrent) => {
  render(DateRange, { showCurrent });
  const invalid = screen.getByRole('textbox', { name: label });
  const other = screen.getByRole('textbox', { name: label === 'Start date' ? 'End date' : 'Start date' });
  await fireEvent.input(invalid, { target: { value: 'abc' } });
  expect(screen.getByText('Use 2024, 2024-03, or 2024-03-15.')).toBeDefined();
  expect(screen.queryByText('2024, 2024-03, or 2024-03-15')).toBeNull();
  await fireEvent.input(other, { target: { value: '2024' } });
  await fireEvent.focusIn(other);
  expect(screen.queryByText('2024, 2024-03, or 2024-03-15')).toBeNull();
  await fireEvent.focusOut(other);
  expect(screen.queryByText('2024, 2024-03, or 2024-03-15')).toBeNull();
});

it('keeps the format hint alongside a different date error', async () => {
  render(DateRange, { showCurrent: false });
  await fireEvent.input(screen.getByRole('textbox', { name: 'Start date' }), { target: { value: '2024-13' } });
  expect(screen.getByText('This date does not exist. Check the month and day.')).toBeDefined();
  expect(screen.getByText('2024, 2024-03, or 2024-03-15')).toBeDefined();
});

it('shows one shared hint with Current shown', async () => {
  const hints = () => screen.queryAllByText('2024, 2024-03, or 2024-03-15');
  render(DateRange, { start: '', end: '', current: true, showCurrent: true });
  expect(hints()).toHaveLength(1);
  const start = screen.getByRole('textbox', { name: 'Start date' });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.input(start, { target: { value: '2024-03' } });
  await fireEvent.focusIn(start);
  expect(hints()).toHaveLength(1);
  await fireEvent.focusOut(start);
  expect(hints()).toHaveLength(0);
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
  expect(hints()).toHaveLength(0);
  await fireEvent.focusIn(end);
  expect(hints()).toHaveLength(1);
});

it('shows one shared hint without Current', async () => {
  const hints = () => screen.queryAllByText('2024, 2024-03, or 2024-03-15');
  render(DateRange, { start: '', end: '', current: false, showCurrent: false });
  expect(hints()).toHaveLength(1);
  const start = screen.getByRole('textbox', { name: 'Start date' });
  const end = screen.getByRole('textbox', { name: 'End date' });
  await fireEvent.focusIn(end);
  expect(hints()).toHaveLength(1);
  await fireEvent.focusOut(end);
  await fireEvent.input(start, { target: { value: '2024-03' } });
  expect(end.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
  await fireEvent.input(end, { target: { value: '2025' } });
  expect(hints()).toHaveLength(0);
});
