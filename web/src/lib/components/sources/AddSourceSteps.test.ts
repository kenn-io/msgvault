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

it('asks for a server name when the Server holds a port', async () => {
  render(AddSourceSteps, { kind: ADD_SOURCE_KINDS.find(kind => kind.id === 'imap') as CommandSourceKind });
  await fireEvent.input(screen.getByLabelText('Username'), { target: { value: 'reader' } });
  await fireEvent.input(screen.getByLabelText('Server'), { target: { value: 'imap.example.com:143' } });
  expect(screen.getByText('Enter a server name, such as imap.example.com.')).toBeDefined();
  expect(screen.queryByText(/msgvault add-imap/, { selector: 'code' })).toBeNull();
  expect(screen.queryByRole('checkbox')).toBeNull();
});
