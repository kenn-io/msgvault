import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import CronField from './CronField.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';

describe('CronField', () => {
  it('describes a valid schedule and tints each field', () => {
    render(CronField, { value: '0 9 * * 1-5', label: 'Schedule' });

    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    expect(input.value).toBe('0 9 * * 1-5');
    expect(input.getAttribute('aria-invalid')).toBeNull();
    expect(screen.getByText('At 09:00 on weekdays')).toBeDefined();
    expect(document.getElementById(input.getAttribute('aria-describedby') ?? '')?.textContent).toBe(
      'At 09:00 on weekdays',
    );
    const tinted = [...document.querySelectorAll('.cron__mirror [data-field]')].map((node) => [
      node.getAttribute('data-field'),
      node.textContent,
    ]);
    expect(tinted).toEqual([
      ['minute', '0'],
      ['hour', '9'],
      ['day', '*'],
      ['month', '*'],
      ['weekday', '1-5'],
    ]);
    expect(screen.getByRole('combobox', { name: 'Presets: Weekdays at 09:00' })).toBeDefined();
  });

  it('flags the broken field while typing and reports the change', async () => {
    const oninput = vi.fn();
    render(CronField, { value: '0 3 * * *', label: 'Schedule', oninput });

    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '0 25 * * *' } });

    expect(oninput).toHaveBeenLastCalledWith('0 25 * * *');
    expect(input.getAttribute('aria-invalid')).toBe('true');
    expect(screen.getByText('Hour: 25 is above the maximum of 23.')).toBeDefined();
    expect(document.querySelector('.cron__mirror [data-field="hour"]')?.getAttribute('data-invalid')).toBe('true');
    expect(document.querySelector('.cron__mirror [data-field="minute"]')?.getAttribute('data-invalid')).toBeNull();
    expect(screen.getByRole('combobox', { name: 'Presets: Custom' })).toBeDefined();
  });

  it('treats an empty optional schedule as off and offers Off in the presets', async () => {
    const oninput = vi.fn();
    render(CronField, { value: '', label: 'Schedule', oninput });

    expect(screen.getByText('Off. Nothing runs on a schedule.')).toBeDefined();
    expect((screen.getByLabelText('Schedule') as HTMLInputElement).getAttribute('aria-invalid')).toBeNull();

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Off' }), 'Every day at 03:00');
    expect(oninput).toHaveBeenLastCalledWith('0 3 * * *');
    expect((screen.getByLabelText('Schedule') as HTMLInputElement).value).toBe('0 3 * * *');
    expect(screen.getByText('At 03:00 every day')).toBeDefined();

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Off');
    expect(oninput).toHaveBeenLastCalledWith('');
  });

  it('asks for a value when the schedule is required', () => {
    render(CronField, { value: '', label: 'Schedule', required: true });

    expect(screen.getByText('Enter a schedule.')).toBeDefined();
    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    expect(input.required).toBe(true);
    expect(input.getAttribute('aria-invalid')).toBe('true');
    expect(screen.queryByRole('option', { name: 'Off' })).toBeNull();
    expect(screen.getByRole('combobox', { name: 'Presets: Custom' })).toBeDefined();
  });
});
