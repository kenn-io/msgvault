import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import { MeetingsController } from '../../meetings/controller.svelte';
import MeetingActionFilters from './MeetingActionFilters.svelte';

it('debounces compact assignee changes for 300 milliseconds', async () => {
  const controller = new MeetingsController({ context: vi.fn(), actions: vi.fn(), metrics: vi.fn() });
  const setFilters = vi.spyOn(controller, 'setFilters').mockResolvedValue(undefined);
  render(MeetingActionFilters, { controller, compact: true });
  vi.useFakeTimers();
  try {
    await fireEvent.input(screen.getByRole('textbox', { name: 'Assignee' }), { target: { value: 'person@example.test' } });
    await vi.advanceTimersByTimeAsync(299);
    expect(setFilters).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(setFilters).toHaveBeenCalledWith({ status: undefined, assigneeEmail: 'person@example.test' });
  } finally { vi.useRealTimers(); controller.destroy(); }
});
