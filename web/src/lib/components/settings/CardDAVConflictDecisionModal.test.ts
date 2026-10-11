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
    { choice: 'keep_local' as const, localState: 'present' as const, remoteState: 'present' as const, action: 'Use msgvault version', consequence: 'Replace this contact in Synthetic contacts with the msgvault version.' },
    { choice: 'keep_remote' as const, localState: 'present' as const, remoteState: 'present' as const, action: 'Use address book version', consequence: 'Use the contact details from Synthetic contacts in msgvault.' },
    { choice: 'keep_local' as const, localState: 'deleted' as const, remoteState: 'present' as const, action: 'Delete from address book', consequence: 'Delete this contact from Synthetic contacts. Other details in msgvault are kept.' },
    { choice: 'keep_remote' as const, localState: 'present' as const, remoteState: 'deleted' as const, action: 'Keep contact deleted', consequence: 'Keep this contact deleted in Synthetic contacts and stop syncing it. msgvault keeps your edits and links to messages or notes. A contact used only by this address book is removed from msgvault.' },
    { choice: 'keep_local' as const, localState: 'present' as const, remoteState: 'deleted' as const, action: 'Restore in address book', consequence: 'Restore this contact in Synthetic contacts using the msgvault version.' },
    { choice: 'keep_remote' as const, localState: 'deleted' as const, remoteState: 'present' as const, action: 'Keep in address book', consequence: "Keep this contact in Synthetic contacts and resume syncing its details with msgvault." }
  ])('explains $choice for msgvault=$localState and address book=$remoteState', ({ choice, localState, remoteState, action, consequence }) => {
    render(CardDAVConflictDecisionModal, {
      addressBookName: 'Synthetic contacts', localState, remoteState, choice,
      pending: false, error: null, onConfirm: vi.fn(), onClose: vi.fn()
    });
    expect(screen.getByRole('dialog', { name: action })).toBeDefined();
    expect(screen.getByText(consequence)).toBeDefined();
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
        addressBookName: 'Synthetic contacts',
        localState: 'present',
        remoteState: 'present',
        choice: 'keep_local',
        pending: false,
        error: null,
        onConfirm,
        onClose
      });
      await waitFor(() => expect(appShortcuts.activeScope()).toBe('carddav-conflict-decision-modal'));

      await fireEvent.click(screen.getByRole('button', { name: 'Use msgvault version' }));
      await waitFor(() => expect(onConfirm).toHaveBeenCalledOnce());
      const dialog = screen.getByRole('dialog', { name: 'Use msgvault version' });
      expect(dialog.querySelector('[aria-busy="true"]')).not.toBeNull();
      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveProperty('disabled', true);
      expect(screen.getByRole('button', { name: 'Saving…' })).toHaveProperty('disabled', true);
      expect(screen.queryByRole('button', { name: 'Close contact choice' })).toBeNull();

      await fireEvent.click(screen.getByRole('button', { name: 'Saving…' }));
      await fireEvent.keyDown(window, { key: 'Escape' });
      await fireEvent.pointerDown(document.querySelector('.kit-modal-overlay')!);
      appShortcuts.handleKeydown(new KeyboardEvent('keydown', { key: 'x', cancelable: true }));
      expect(onConfirm).toHaveBeenCalledOnce();
      expect(onClose).not.toHaveBeenCalled();
      expect(rootShortcut).not.toHaveBeenCalled();

      deferred.resolve();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Use msgvault version' })).toHaveProperty('disabled', false));
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(onClose).toHaveBeenCalledOnce();
    } finally {
      unregister();
    }
  });

  it('keeps a fixed resolution error in the modal for an explicit fresh confirmation', () => {
    render(CardDAVConflictDecisionModal, {
      addressBookName: 'Synthetic contacts',
      localState: 'present',
      remoteState: 'present',
      choice: 'keep_remote',
      pending: false,
      error: 'Unable to save your choice. Try again.',
      onConfirm: vi.fn(),
      onClose: vi.fn()
    });

    expect(screen.getByRole('alert').textContent).toBe('Unable to save your choice. Try again.');
    expect(screen.getByRole('button', { name: 'Use address book version' })).toHaveProperty('disabled', false);
  });
});
