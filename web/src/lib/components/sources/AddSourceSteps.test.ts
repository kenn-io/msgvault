import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import AddSourceSteps from './AddSourceSteps.svelte';
import { ADD_SOURCE_KINDS, type CommandSourceKind } from '../../sources/add-source';

it('shows the IMAP steps and one command when platforms agree', async () => {
  render(AddSourceSteps, { kind: ADD_SOURCE_KINDS.find(kind => kind.id === 'imap') as CommandSourceKind });
  await fireEvent.input(screen.getByLabelText('Server'), { target: { value: 'imap.example.com' } });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'reader' } });
  expect(screen.getByText('The command asks for your password.')).toBeDefined();
  expect(screen.getAllByRole('listitem').map(item => item.textContent)).toEqual(['Run this command in a terminal where msgvault is installed.', 'Run the first-sync command it prints.']);
  expect(screen.getByRole('link', { name: 'Setup guide' })).toBeDefined();
  expect(screen.queryByRole('radiogroup', { name: 'Platform' })).toBeNull();
  const command = screen.getByText(/imap\.example\.com/, { selector: 'code' });
  expect(command.textContent).toContain('reader');
});

it('uses the port supplied with the IMAP server', async () => {
  render(AddSourceSteps, { kind: ADD_SOURCE_KINDS.find(kind => kind.id === 'imap') as CommandSourceKind });
  const server = screen.getByLabelText('Server');
  await fireEvent.input(server, { target: { value: 'imap.example.com:993' } });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'reader' } });
  expect(screen.getByText('msgvault add-imap --host imap.example.com --port 993 --username reader', { selector: 'code' })).toBeDefined();
  await fireEvent.input(server, { target: { value: 'imap.example.com:70000' } });
  expect(screen.getByText('Enter a port from 1 to 65535.')).toBeDefined();
  expect(screen.queryByText(/msgvault add-imap/, { selector: 'code' })).toBeNull();
});
