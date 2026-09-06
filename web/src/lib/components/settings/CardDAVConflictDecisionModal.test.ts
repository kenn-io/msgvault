import { appShortcuts } from '@kenn-io/kit-ui';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import CardDAVConflictDecisionModal from './CardDAVConflictDecisionModal.svelte';

function deferredVoid() {
  let resolve!: () => void;
  const promise = new Promise<void>((settle) => { resolve = settle; });
  return { promise, resolve };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('CardDAVConflictDecisionModal', () => {
  it.each([
    { choice: 'keep_local' as const, side: 'local', action: 'Keep local card' },
    { choice: 'keep_remote' as const, side: 'remote', action: 'Keep remote card' }
  ])('names the $side whole-card choice and explains deleted-side tombstones', ({ choice, side, action }) => {
    render(CardDAVConflictDecisionModal, {
      conflictID: 41,
      choice,
      pending: false,
      error: null,
      onConfirm: vi.fn(),
      onClose: vi.fn()
    });

    expect(screen.getByRole('dialog', { name: `Keep ${side} CardDAV card` })).toBeDefined();
    expect(screen.getByText(new RegExp(`keep the ${side} side for the whole card`, 'i'))).toBeDefined();
    expect(screen.getByText(/A deleted side is a tombstone/i)).toBeDefined();
    expect(screen.getByRole('button', { name: action })).toBeDefined();
  });

  it('blocks every dismissal path, duplicate confirm, and root shortcut while confirmation is pending', async () => {
    const deferred = deferredVoid();
    const onConfirm = vi.fn(() => deferred.promise);
    const onClose = vi.fn();
    const rootShortcut = vi.fn();
    const unregister = appShortcuts.register('x', rootShortcut);
    try {
      render(CardDAVConflictDecisionModal, {
        conflictID: 41,
        choice: 'keep_local',
        pending: false,
        error: null,
        onConfirm,
        onClose
      });
      await waitFor(() => expect(appShortcuts.activeScope()).toBe('carddav-conflict-decision-modal'));

      await fireEvent.click(screen.getByRole('button', { name: 'Keep local card' }));
      await waitFor(() => expect(onConfirm).toHaveBeenCalledOnce());
      const dialog = screen.getByRole('dialog', { name: 'Keep local CardDAV card' });
      expect(dialog.querySelector('[aria-busy="true"]')).not.toBeNull();
      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Keeping local card…' })).toHaveProperty('disabled', true);
      expect(screen.queryByRole('button', { name: 'Close CardDAV conflict decision' })).toBeNull();

      await fireEvent.click(screen.getByRole('button', { name: 'Keeping local card…' }));
      await fireEvent.keyDown(window, { key: 'Escape' });
      await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(onConfirm).toHaveBeenCalledOnce();
      expect(onClose).not.toHaveBeenCalled();
      expect(rootShortcut).not.toHaveBeenCalled();

      deferred.resolve();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Keep local card' })).toHaveProperty('disabled', false));
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onClose).toHaveBeenCalledOnce();
    } finally {
      unregister();
    }
  });

  it('keeps a fixed resolution error in the modal for an explicit fresh confirmation', () => {
    render(CardDAVConflictDecisionModal, {
      conflictID: 41,
      choice: 'keep_remote',
      pending: false,
      error: 'Unable to resolve this CardDAV conflict.',
      onConfirm: vi.fn(),
      onClose: vi.fn()
    });

    expect(screen.getByRole('alert').textContent).toBe('Unable to resolve this CardDAV conflict.');
    expect(screen.getByRole('button', { name: 'Keep remote card' })).toHaveProperty('disabled', false);
  });
});
