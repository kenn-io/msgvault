import { getContext, onDestroy, setContext } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';

/** A Settings form that saves through its own button instead of Save changes. */
export interface UnsavedForm {
  dirty(): boolean;
  discard(): void;
}

const KEY = Symbol('msgvault:settings-unsaved-forms');

/** Lets leaving Settings warn about, and then drop, edits in self-saving forms. */
export class UnsavedForms {
  private readonly forms = new SvelteSet<UnsavedForm>();

  get any(): boolean {
    for (const form of this.forms) if (form.dirty()) return true;
    return false;
  }

  discard(): void {
    for (const form of this.forms) form.discard();
  }

  add(form: UnsavedForm): () => void {
    this.forms.add(form);
    return () => this.forms.delete(form);
  }
}

export function provideUnsavedForms(): UnsavedForms {
  return setContext(KEY, new UnsavedForms());
}

/** Registers a form with the enclosing Settings page for as long as it is mounted. */
export function trackUnsavedForm(form: UnsavedForm): void {
  const forms = getContext<UnsavedForms | undefined>(KEY);
  if (forms) onDestroy(forms.add(form));
}
