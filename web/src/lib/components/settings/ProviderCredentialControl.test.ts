import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import ProviderCredentialControl from './ProviderCredentialControl.svelte';

it('stages a stored override of an environment key and discards it without clearing the environment value', async () => {
  const onchange = vi.fn();
  const props = { label: 'API key', credentialState: { configured: true, source: 'environment' as const, hint: 'env…key' }, onchange };
  const { rerender } = render(ProviderCredentialControl, props);
  const replace = screen.getByRole('button', { name: 'Replace API key' }) as HTMLButtonElement;
  expect(replace.disabled).toBe(false);
  expect(screen.queryByRole('button', { name: 'Clear API key' })).toBeNull();
  await fireEvent.click(replace);
  await fireEvent.input(screen.getByLabelText('New API key'), { target: { value: 'synthetic-override' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Use API key' }));
  expect(onchange).toHaveBeenCalledWith({ action: 'set', value: 'synthetic-override' });
  await rerender({ ...props, pending: { action: 'set', value: 'synthetic-override' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Clear API key' }));
  expect(onchange).toHaveBeenLastCalledWith(undefined);
});
