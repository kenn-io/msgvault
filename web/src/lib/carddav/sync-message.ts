export function cardDAVBindingConflictMessage(count: number): string {
  return count > 0 ? `${count} CardDAV binding conflicts. Check CardDAV settings for the review command.` : '';
}
