// Unicode White_Space matches Go's unicode.IsSpace set used by
// strings.TrimSpace. In particular it trims U+0085 but preserves U+FEFF.
export function trimAttributeText(value: string): string {
  return value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '');
}

// Array iteration counts Unicode code points, matching Go's []rune length.
export function attributeTextLength(value: string): number {
  return [...value].length;
}
