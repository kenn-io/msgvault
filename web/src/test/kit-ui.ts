import { fireEvent, screen } from '@testing-library/svelte';

export async function chooseSelectOption(trigger: HTMLElement, optionName: string): Promise<void> {
  await fireEvent.click(trigger);
  await fireEvent.click(await screen.findByRole('option', { name: optionName }));
}

// The closed trigger is named "<placeholder>: <selected label>" (or the
// placeholder alone with nothing to show), so match on the placeholder prefix.
export async function openTypeahead(triggerName: string): Promise<HTMLInputElement> {
  const escaped = triggerName.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  await fireEvent.click(screen.getByRole('button', { name: new RegExp(`^${escaped}(: |$)`) }));
  return screen.getByRole('combobox', { name: triggerName }) as HTMLInputElement;
}
