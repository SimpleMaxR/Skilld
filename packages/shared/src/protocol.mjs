export const INSTALL_STATUS = Object.freeze({
  SYNCED: 'synced',
  OUTDATED: 'outdated',
  MODIFIED: 'modified',
  UNMANAGED: 'unmanaged',
  MISSING: 'missing',
});

export const ACTION_TYPE = Object.freeze({
  INSTALL: 'install',
  UPDATE: 'update',
  RESET: 'reset',
  REMOVE: 'remove',
  ADOPT_CONFIRM: 'adopt_confirm',
  FORGET_STATE: 'forget_state',
});

export const ACTION_STATUS = Object.freeze({
  PENDING: 'pending',
  CLAIMED: 'claimed',
  COMPLETED: 'completed',
  FAILED: 'failed',
  CANCELLED: 'cancelled',
});

/**
 * Convert an arbitrary skill name or title into a URL/filesystem-safe slug id.
 */
export function slugifySkillId(input) {
  const cleaned = String(input ?? '')
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, '-')
    .replace(/-{2,}/g, '-')
    .replace(/^[-_]+|[-_]+$/g, '');
  return cleaned || 'skill';
}

/**
 * Validate a relative POSIX path inside a skill bundle directory.
 */
export function isValidRelativePath(relPath) {
  if (typeof relPath !== 'string' || relPath.length === 0) return false;
  if (relPath.includes('\0') || relPath.includes('\\')) return false;
  if (relPath.startsWith('/') || relPath.endsWith('/')) return false;
  const parts = relPath.split('/');
  for (const part of parts) {
    if (!part || part === '.' || part === '..') return false;
  }
  return true;
}

/**
 * Return a short human-readable prefix of a content hash or git commit sha.
 */
export function shortHash(hash, length = 10) {
  if (!hash) return '—';
  const raw = String(hash).replace(/^sha256:/, '');
  return raw.slice(0, length);
}

/**
 * Return current ISO-8601 UTC timestamp.
 */
export function isoNow() {
  return new Date().toISOString();
}

/**
 * Generate a collision-resistant identifier with optional prefix.
 */
export function generateId(prefix = '') {
  const uuid =
    typeof globalThis.crypto?.randomUUID === 'function'
      ? globalThis.crypto.randomUUID()
      : Array.from(globalThis.crypto.getRandomValues(new Uint8Array(16)))
          .map((b) => b.toString(16).padStart(2, '0'))
          .join('');
  return prefix ? `${prefix}_${uuid}` : uuid;
}
