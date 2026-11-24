import { isValidRelativePath } from './protocol.mjs';

const textEncoder = new TextEncoder();

/**
 * Normalize a skill file map into canonical sorted shape:
 * { [relPath]: { encoding: 'utf8' | 'base64', content: string, mode: string } }
 * Throws if any path is unsafe or invalid.
 */
export function normalizeFileMap(files) {
  if (!files || typeof files !== 'object' || Array.isArray(files)) {
    throw new TypeError('Skill files must be a plain object mapping relative paths to entries');
  }

  const keys = Object.keys(files).sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  const normalized = {};

  for (const relPath of keys) {
    if (!isValidRelativePath(relPath)) {
      throw new Error(`Invalid relative skill file path: "${relPath}"`);
    }
    const entry = files[relPath];
    if (typeof entry === 'string') {
      normalized[relPath] = {
        encoding: 'utf8',
        content: entry,
        mode: '0644',
      };
      continue;
    }
    if (!entry || typeof entry !== 'object') {
      throw new TypeError(`Invalid file entry for "${relPath}"`);
    }
    const encoding = entry.encoding === 'base64' ? 'base64' : 'utf8';
    const content = typeof entry.content === 'string' ? entry.content : '';
    const mode = normalizeMode(entry.mode);
    normalized[relPath] = { encoding, content, mode };
  }

  return normalized;
}

function normalizeMode(mode) {
  if (typeof mode === 'number') {
    return (mode & 0o111) !== 0 ? '0755' : '0644';
  }
  const str = String(mode ?? '0644').trim();
  if (str === '0755' || str === '755' || str === '100755') return '0755';
  return '0644';
}

/**
 * Compute deterministic SHA-256 hash over a skill's canonical file map.
 * Compatible with both Node.js and Cloudflare Workers via Web Crypto API.
 */
export async function computeFilesHash(files) {
  const canonical = normalizeFileMap(files);
  const chunks = [];
  let totalBytes = 0;

  for (const relPath of Object.keys(canonical)) {
    const entry = canonical[relPath];
    const header = textEncoder.encode(`${relPath}\0${entry.encoding}\0${entry.mode}\0`);
    const body = textEncoder.encode(entry.content);
    const footer = textEncoder.encode('\n');
    chunks.push(header, body, footer);
    totalBytes += header.byteLength + body.byteLength + footer.byteLength;
  }

  const buffer = new Uint8Array(totalBytes);
  let offset = 0;
  for (const chunk of chunks) {
    buffer.set(chunk, offset);
    offset += chunk.byteLength;
  }

  const digest = await globalThis.crypto.subtle.digest('SHA-256', buffer);
  const hex = Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('');
  return `sha256:${hex}`;
}
