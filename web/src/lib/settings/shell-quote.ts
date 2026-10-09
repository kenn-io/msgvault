export function shellQuote(value: string, shell: 'posix' | 'cmd'): string {
  if (shell === 'cmd') return cmdQuote(value);
  return "'" + value.replaceAll("'", "'\\''") + "'";
}

// Requires cmd /d /v:off; split % runs to prevent expansion and escape quotes for both cmd and Windows argv.
function cmdQuote(value: string): string {
  let quoted = '"';
  let slashes = 0;
  for (const char of value) {
    if (char === '\\') {
      slashes++;
      continue;
    }
    if (char === '"' || char === '%') {
      quoted += '\\'.repeat(slashes * 2) + '"' + (char === '"' ? '\\^"' : char) + '"';
    } else {
      quoted += '\\'.repeat(slashes) + char;
    }
    slashes = 0;
  }
  return quoted + '\\'.repeat(slashes * 2) + '"';
}
