import { fireEvent, screen } from '@testing-library/svelte';

export async function chooseSelectOption(trigger: HTMLElement, optionName: string): Promise<void> {
  await fireEvent.click(trigger);
  await fireEvent.click(await screen.findByRole('option', { name: optionName }));
}

export async function openTypeahead(triggerName: string): Promise<HTMLInputElement> {
  await fireEvent.click(screen.getByRole('button', { name: triggerName }));
  return screen.getByRole('combobox', { name: triggerName }) as HTMLInputElement;
}
