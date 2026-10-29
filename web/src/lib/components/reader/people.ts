/** A person as the reader shows them: the name when there is one, the address on hover. */
export interface PersonLabel {
  name: string;
  address: string;
}

// "Sarah Benson <sarah@example.com>" splits into both halves; a bare address is its own name.
export function personLabel(value: string): PersonLabel {
  const match = /^\s*"?(.*?)"?\s*<([^>]+)>\s*$/.exec(value);
  if (match) {
    const address = match[2]!.trim();
    return { name: match[1]!.trim() || address, address };
  }
  const bare = value.trim();
  return { name: bare, address: bare };
}
