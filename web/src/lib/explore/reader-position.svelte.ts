const STORAGE_KEY = 'msgvault.reading-pane.position';

function stored(): 'below' | 'right' {
  try {
    return localStorage.getItem(STORAGE_KEY) === 'right' ? 'right' : 'below';
  } catch {
    return 'below';
  }
}

/**
 * Where the reader opens, under the list or beside it. The Display menu sets it
 * and the message list reads it; the choice outlives the page in browser storage.
 */
class ReaderPosition {
  #value = $state<'below' | 'right'>(stored());

  get value(): 'below' | 'right' {
    return this.#value;
  }

  set(value: string): void {
    this.#value = value === 'right' ? 'right' : 'below';
    try {
      localStorage.setItem(STORAGE_KEY, this.#value);
    } catch {
      // The choice still holds for this page without browser storage.
    }
  }
}

export const readerPosition = new ReaderPosition();
