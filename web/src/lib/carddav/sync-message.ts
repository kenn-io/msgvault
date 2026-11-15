export function cardDAVBindingConflictMessage(count: number): string {
  return count > 0 ? `${count} CardDAV binding conflicts. Run msgvault carddav bind-participants --dry-run for details.` : '';
}
