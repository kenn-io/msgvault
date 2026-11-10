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

it.each([true, false])('shows one shared date hint with Current shown: %s', async showCurrent => {
  const hints = () => screen.queryAllByText('2024, 2024-03, or 2024-03-15');
  render(DateRange, { start: '', end: '', current: showCurrent, showCurrent });
  expect(hints()).toHaveLength(1);
  const start = screen.getByRole('textbox', { name: 'Start date' });
  const end = screen.getByRole('textbox', { name: 'End date' });
  if (showCurrent) {
    await fireEvent.input(start, { target: { value: '2024-03' } });
    await fireEvent.focusIn(start);
    expect(hints()).toHaveLength(1);
    await fireEvent.focusOut(start);
    expect(hints()).toHaveLength(0);
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Current' }));
    expect(hints()).toHaveLength(0);
    await fireEvent.focusIn(end);
    expect(hints()).toHaveLength(1);
  } else {
    await fireEvent.focusIn(end);
    expect(hints()).toHaveLength(1);
    expect(end.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
    await fireEvent.focusOut(end);
    await fireEvent.input(start, { target: { value: '2024-03' } });
    expect(end.closest('.field')!.textContent).toContain('2024, 2024-03, or 2024-03-15');
    await fireEvent.input(end, { target: { value: '2025' } });
    expect(hints()).toHaveLength(0);
  }
});
