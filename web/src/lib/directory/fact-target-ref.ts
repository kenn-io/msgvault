export interface FactTargetRef {
  kind: 'attribute' | 'employment';
  key: string;
  revision: string;
}

const REVISION_PATTERN = /^sha256:[0-9a-f]{64}$/;

export function encodeFactTargetRef(target: {
  kind?: string;
  key?: string;
  revision?: string;
}): string | undefined {
  if (target.kind !== 'attribute' && target.kind !== 'employment') return undefined;
  if (!target.key || target.key.trim() !== target.key) return undefined;
  if (!target.revision || !REVISION_PATTERN.test(target.revision)) return undefined;
  return `${target.kind}:${target.key}:${target.revision}`;
}

export function decodeFactTargetRef(value: string): FactTargetRef | undefined {
  if (value.trim() !== value) return undefined;
  const revisionMarker = value.lastIndexOf(':sha256:');
  if (revisionMarker < 0) return undefined;
  const revision = value.slice(revisionMarker + 1);
  const prefix = value.slice(0, revisionMarker);
  const kindBoundary = prefix.indexOf(':');
  if (kindBoundary < 0) return undefined;
  const kind = prefix.slice(0, kindBoundary);
  const key = prefix.slice(kindBoundary + 1);
  const encoded = encodeFactTargetRef({ kind, key, revision });
  if (encoded !== value) return undefined;
  return { kind: kind as FactTargetRef['kind'], key, revision };
}
