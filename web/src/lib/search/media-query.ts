export function admitsMediaQuery(query: string): boolean {
  return query.trim().split(/\s+/u).every(term => /^[\p{L}\p{M}\p{N}]+$/u.test(term) && /[\p{L}\p{N}]/u.test(term));
}

const UNREADABLE_QUERY = 'Recording search cannot read this query. Check archive access or change the query.';

export function attachmentScopeNotice(participantID: number | undefined): string {
  return participantID === undefined
    ? 'Attachment search supports at most 4096 mirrored files excluding images, audio, and video. Add a Person filter to search attachments.'
    : 'This Person filter exceeds the attachment-search limit of 4096 mirrored files excluding images, audio, and video. Choose a Person filter with fewer attachments.';
}

export function refusedMediaSearchNotice(
  code: string | undefined, message: string | undefined, query: string, participantID: number | undefined,
): string {
  switch (code) {
    case 'media_search_unavailable':
      return 'Recording search unavailable.';
    case 'media_search_scope_limit':
      return participantID === undefined
        ? 'Add a Person filter to search recordings.'
        : 'This Person filter exceeds recording-search limits. Use person-scoped recording search in the CLI.';
    case 'invalid_media_search':
      return 'Recording search rejected this query. Try different plain words.';
    case 'invalid_parameter':
      if (new TextEncoder().encode(query).length > 8192) return 'Recording search query is too long. Use fewer words.';
      return message || UNREADABLE_QUERY;
    default:
      return UNREADABLE_QUERY;
  }
}
