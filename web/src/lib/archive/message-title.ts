export function messageTitle(subject?: string, conversationTitle?: string): string {
  return subject || conversationTitle || '';
}
