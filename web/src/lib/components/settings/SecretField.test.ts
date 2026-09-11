import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import SecretField from './SecretField.svelte';

describe('SecretField', () => {
  it('shows None with an Add button when nothing is set', () => {
    render(SecretField, { label: 'Task API key', onreplace: () => true });

    expect(screen.getByLabelText('Task API key').textContent).toBe('None');
    expect(screen.getByRole('button', { name: 'Add task API key' })).toBeDefined();
    expect(screen.queryByRole('button', { name: /Clear/ })).toBeNull();
  });

  it('shows the masked hint, a Replace button, and a trash button for a set key', () => {
    const onclear = vi.fn();
    render(SecretField, { label: 'Task API key', configured: true, hint: 'sk-…x9Q', onreplace: () => true, onclear });

    expect(screen.getByLabelText('Task API key').textContent).toBe('sk-…x9Q');
    expect(screen.getByRole('button', { name: 'Replace task API key' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Clear task API key' }));
    expect(onclear).toHaveBeenCalledTimes(1);
  });

  it('marks a set key without a hint and names where an environment key comes from', () => {
    render(SecretField, { label: 'Task API key', configured: true, source: 'environment', onreplace: () => true });

    expect(screen.getByLabelText('Task API key').textContent).toBe('••••••••');
    expect(screen.getByText('From an environment variable on the daemon host.')).toBeDefined();
  });

  it('takes a new key through the dialog and closes when it is accepted', async () => {
    const onreplace = vi.fn(() => true);
    render(SecretField, { label: 'Task API key', applyNote: 'Applies right away.', onreplace });

    await fireEvent.click(screen.getByRole('button', { name: 'Add task API key' }));
    const dialog = screen.getByRole('dialog', { name: 'Add task API key' });
    expect(dialog.textContent).toContain('Applies right away.');
    const save = screen.getByRole('button', { name: 'Save task API key' }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    await fireEvent.input(screen.getByLabelText('New task API key'), { target: { value: 'sk-live-0123456789' } });
    expect(save.disabled).toBe(false);
    await fireEvent.click(save);
    expect(onreplace).toHaveBeenCalledWith('sk-live-0123456789');
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('keeps the dialog open with the error when the key is refused', async () => {
    const onreplace = vi.fn(async () => false);
    const { rerender } = render(SecretField, { label: 'Task API key', onreplace });

    await fireEvent.click(screen.getByRole('button', { name: 'Add task API key' }));
    await fireEvent.input(screen.getByLabelText('New task API key'), { target: { value: 'refused-key-value' } });
    await fireEvent.submit(screen.getByLabelText('New task API key').closest('form') as HTMLFormElement);
    await rerender({ label: 'Task API key', onreplace, error: 'Unable to save provider credential.' });
    expect(screen.getByRole('dialog', { name: 'Add task API key' })).toBeDefined();
    expect(screen.getByRole('alert').textContent).toBe('Unable to save provider credential.');

    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('blocks the buttons and says why while the field is disabled', async () => {
    render(SecretField, {
      label: 'Task API key',
      configured: true,
      hint: 'sk-…x9Q',
      disabledReason: 'Save endpoint settings first.',
      onreplace: () => true,
      onclear: () => undefined,
    });

    expect((screen.getByRole('button', { name: 'Replace task API key' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Clear task API key' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText('Save endpoint settings first.')).toBeDefined();
    await fireEvent.click(screen.getByRole('button', { name: 'Replace task API key' }));
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
