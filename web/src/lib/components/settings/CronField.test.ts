import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import CronField from './CronField.svelte';
import { chooseSelectOption } from '../../../test/kit-ui';

describe('CronField', () => {
  it('shows a preset as one menu and hides the expression editor', () => {
    render(CronField, { value: '0 9 * * 1-5', label: 'Schedule' });

    expect(screen.getByRole('combobox', { name: 'Presets: Weekdays at 09:00' })).toBeDefined();
    expect(screen.queryByLabelText('Schedule')).toBeNull();
    expect(screen.getByRole('button', { name: /^Time zone/ }).textContent).toContain('Server time');
    expect(screen.getByText('At 09:00 on weekdays').classList.contains('kit-sr-only')).toBe(true);
  });

  it('opens the editor for a custom expression and tints each field', () => {
    render(CronField, { value: '0 4 * * *', label: 'Schedule' });

    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    expect(input.value).toBe('0 4 * * *');
    expect(input.getAttribute('aria-invalid')).toBeNull();
    expect(screen.getByRole('combobox', { name: 'Presets: Custom' })).toBeDefined();
    expect(document.getElementById(input.getAttribute('aria-describedby') ?? '')?.textContent?.trim()).toBe(
      'At 04:00 every day',
    );
    const tinted = [...document.querySelectorAll('.cron__mirror [data-field]')].map((node) => [
      node.getAttribute('data-field'),
      node.textContent,
    ]);
    expect(tinted).toEqual([
      ['minute', '0'],
      ['hour', '4'],
      ['day', '*'],
      ['month', '*'],
      ['weekday', '*'],
    ]);
    const legend = document.querySelector('.cron__legend');
    expect(legend?.getAttribute('aria-hidden')).toBe('true');
    expect([...(legend?.querySelectorAll('[data-field]') ?? [])].map((node) => node.textContent)).toEqual([
      'minute',
      'hour',
      'day',
      'month',
      'weekday',
    ]);
    expect(legend?.textContent).toContain('At 04:00 every day');
  });

  it('keeps the editor open after choosing Custom from a preset', async () => {
    const oninput = vi.fn();
    render(CronField, { value: '*/15 * * * *', label: 'Schedule', oninput });

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every 15 minutes' }), 'Custom');
    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    expect(input.value).toBe('*/15 * * * *');
    expect(oninput).not.toHaveBeenCalled();

    await fireEvent.input(input, { target: { value: '0 25 * * *' } });
    expect(oninput).toHaveBeenLastCalledWith('0 25 * * *');
    expect(input.getAttribute('aria-invalid')).toBe('true');
    const status = screen.getByText('Hour: 25 is above the maximum of 23.');
    expect(status.classList.contains('kit-sr-only')).toBe(false);
    expect(document.querySelector('.cron__mirror [data-field="hour"]')?.getAttribute('data-invalid')).toBe('true');
    expect(document.querySelector('.cron__mirror [data-field="minute"]')?.getAttribute('data-invalid')).toBeNull();
  });

  it('keeps the editor open while typing through a preset and follows an outside reset', async () => {
    const oninput = vi.fn();
    const { rerender } = render(CronField, { value: '0 4 * * *', label: 'Schedule', oninput });

    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '0 3 * * *' } });
    expect(oninput).toHaveBeenLastCalledWith('0 3 * * *');
    expect(screen.getByRole('combobox', { name: 'Presets: Custom' })).toBeDefined();
    expect((screen.getByLabelText('Schedule') as HTMLInputElement).value).toBe('0 3 * * *');

    // Discard restores the stored preset from outside: the menu names it.
    await rerender({ value: '*/15 * * * *', label: 'Schedule', oninput });
    expect(screen.getByRole('combobox', { name: 'Presets: Every 15 minutes' })).toBeDefined();
    expect(screen.queryByLabelText('Schedule')).toBeNull();
  });

  it('does not reopen the editor when the value returns to the latched text or the zone changes', async () => {
    const oninput = vi.fn();
    const { rerender } = render(CronField, { value: '0 4 * * *', label: 'Schedule', oninput });

    await fireEvent.input(screen.getByLabelText('Schedule'), { target: { value: '0 3 * * *' } });
    expect(screen.getByRole('combobox', { name: 'Presets: Custom' })).toBeDefined();

    // Another client stores an hourly run, then puts the daily run back.
    await rerender({ value: '0 * * * *', label: 'Schedule', oninput });
    await rerender({ value: '0 3 * * *', label: 'Schedule', oninput });
    expect(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' })).toBeDefined();
    expect(screen.queryByLabelText('Schedule')).toBeNull();

    // Changing only the zone after an outside reset keeps the preset by name.
    await fireEvent.click(screen.getByRole('button', { name: /^Time zone/ }));
    await fireEvent.input(screen.getByRole('combobox', { name: 'Time zone' }), { target: { value: 'utc' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /^UTC/ }));
    expect(oninput).toHaveBeenLastCalledWith('CRON_TZ=UTC 0 3 * * *');
    expect(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' })).toBeDefined();
    expect(screen.queryByLabelText('Schedule')).toBeNull();
  });

  it('shows the preset by name when Custom is discarded back to a preset', async () => {
    const { rerender } = render(CronField, { value: '0 3 * * *', label: 'Schedule' });

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Custom');
    expect(screen.getByLabelText('Schedule')).toBeDefined();

    await rerender({ value: '0 * * * *', label: 'Schedule' });
    expect(screen.getByRole('combobox', { name: 'Presets: Every hour' })).toBeDefined();
    expect(screen.queryByLabelText('Schedule')).toBeNull();
  });

  it('treats an empty optional schedule as off and starts Custom from a daily run', async () => {
    const oninput = vi.fn();
    render(CronField, { value: '', label: 'Schedule', oninput });

    expect(screen.getByRole('combobox', { name: 'Presets: Off' })).toBeDefined();
    expect(screen.queryByLabelText('Schedule')).toBeNull();
    expect(screen.queryByRole('button', { name: /^Time zone/ })).toBeNull();

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Off' }), 'Every day at 03:00');
    expect(oninput).toHaveBeenLastCalledWith('0 3 * * *');
    expect(screen.getByRole('button', { name: /^Time zone/ })).toBeDefined();

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Off');
    expect(oninput).toHaveBeenLastCalledWith('');

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Off' }), 'Custom');
    expect(oninput).toHaveBeenLastCalledWith('0 3 * * *');
    expect((screen.getByLabelText('Schedule') as HTMLInputElement).value).toBe('0 3 * * *');
  });

  it('stores a chosen time zone as a prefix and keeps the fields on their own', async () => {
    const oninput = vi.fn();
    render(CronField, { value: 'CRON_TZ=Europe/Berlin 0 4 * * *', label: 'Schedule', oninput });

    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    expect(input.value).toBe('0 4 * * *');
    expect(screen.getByText('At 04:00 every day, Europe/Berlin time')).toBeDefined();

    await fireEvent.click(screen.getByRole('button', { name: /^Time zone/ }));
    await fireEvent.input(screen.getByRole('combobox', { name: 'Time zone' }), { target: { value: 'tokyo' } });
    await fireEvent.mouseDown(await screen.findByRole('option', { name: /Asia\/Tokyo/ }));
    expect(oninput).toHaveBeenLastCalledWith('CRON_TZ=Asia/Tokyo 0 4 * * *');
    expect(input.value).toBe('0 4 * * *');

    await fireEvent.input(input, { target: { value: '0 5 * * *' } });
    expect(oninput).toHaveBeenLastCalledWith('CRON_TZ=Asia/Tokyo 0 5 * * *');

    await fireEvent.click(screen.getByRole('button', { name: /^Time zone/ }));
    await fireEvent.mouseDown(await screen.findByRole('option', { name: 'Server time' }));
    expect(oninput).toHaveBeenLastCalledWith('0 5 * * *');
  });

  it('shows a stored zone the browser list lacks and stores a trimmed expression', async () => {
    const oninput = vi.fn();
    render(CronField, { value: 'CRON_TZ=US/Eastern 0 4 * * *', label: 'Schedule', oninput });

    expect(screen.getByRole('button', { name: /^Time zone/ }).textContent).toContain('US/Eastern');

    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: '  0 6 * * *  ' } });
    expect(input.value).toBe('  0 6 * * *  ');
    expect(oninput).toHaveBeenLastCalledWith('CRON_TZ=US/Eastern 0 6 * * *');

    await fireEvent.input(input, { target: { value: '   ' } });
    expect(oninput).toHaveBeenLastCalledWith('');
  });

  it('keeps the zone while the expression is empty and drops it from the stored value', async () => {
    const oninput = vi.fn();
    render(CronField, { value: 'CRON_TZ=UTC 0 3 * * *', label: 'Schedule', oninput });

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Every day at 03:00' }), 'Off');
    expect(oninput).toHaveBeenLastCalledWith('');
    expect(screen.queryByRole('button', { name: /^Time zone/ })).toBeNull();

    await chooseSelectOption(screen.getByRole('combobox', { name: 'Presets: Off' }), 'Every hour');
    expect(oninput).toHaveBeenLastCalledWith('CRON_TZ=UTC 0 * * * *');
    expect(screen.getByRole('button', { name: /^Time zone/ }).textContent).toContain('UTC');
  });

  it('asks for a value when the schedule is required', () => {
    render(CronField, { value: '', label: 'Schedule', required: true });

    expect(screen.getByText('Enter a schedule.').classList.contains('kit-sr-only')).toBe(false);
    const input = screen.getByLabelText('Schedule') as HTMLInputElement;
    expect(input.required).toBe(true);
    expect(input.getAttribute('aria-invalid')).toBe('true');
    expect(screen.queryByRole('option', { name: 'Off' })).toBeNull();
    expect(screen.getByRole('combobox', { name: 'Presets: Custom' })).toBeDefined();
  });
});
