import {
  computeFilesHash,
  isoNow,
  normalizeFileMap,
  parseSkillFrontmatter,
} from '../../shared/src/index.mjs';
import { HttpError } from './router.mjs';

const MAX_FILE_SIZE_BYTES = 2 * 1024 * 1024;
const IGNORED_SEGMENTS = new Set(['.git', 'node_modules', '__pycache__', '.DS_Store']);
const textDecoderFatal = new TextDecoder('utf-8', { fatal: true });

/**
 * Parse a GitHub repository or subdirectory URL / shorthand into { owner, repo, ref, subpath }.
 */
export function parseGitHubUrl(input, explicitRef = null, explicitSubpath = null) {
  const raw = String(input ?? '').trim();
  if (!raw) {
    throw new HttpError(400, 'GitHub URL is required', 'INVALID_GITHUB_URL');
  }

  let owner = '';
  let repo = '';
  let ref = explicitRef ? String(explicitRef).trim() : null;
  let subpath = explicitSubpath ? cleanSubpath(explicitSubpath) : '';

  if (/^https?:\/\//i.test(raw)) {
    let url;
    try {
      url = new URL(raw);
    } catch {
      throw new HttpError(400, 'Malformed GitHub URL', 'INVALID_GITHUB_URL');
    }
    if (!/^(www\.)?github\.com$/i.test(url.hostname)) {
      throw new HttpError(400, 'Only github.com URLs are supported', 'INVALID_GITHUB_URL');
    }
    const parts = url.pathname.split('/').filter(Boolean);
    if (parts.length < 2) {
      throw new HttpError(400, 'GitHub URL must include owner and repository', 'INVALID_GITHUB_URL');
    }
    owner = parts[0];
    repo = parts[1].replace(/\.git$/i, '');

    if (parts[2] === 'tree' && parts.length >= 4) {
      ref = ref || decodeURIComponent(parts[3]);
      if (!subpath && parts.length > 4) {
        subpath = cleanSubpath(parts.slice(4).map(decodeURIComponent).join('/'));
      }
    } else if (parts[2] === 'blob' && parts.length >= 5) {
      ref = ref || decodeURIComponent(parts[3]);
      const rest = parts.slice(4).map(decodeURIComponent);
      if (rest[rest.length - 1] === 'SKILL.md') {
        rest.pop();
      }
      if (!subpath) {
        subpath = cleanSubpath(rest.join('/'));
      }
    }
  } else {
    const parts = raw.replace(/^\/+|\/+$/g, '').split('/').filter(Boolean);
    if (parts.length < 2) {
      throw new HttpError(
        400,
        'Provide a GitHub URL or owner/repo[/subpath]',
        'INVALID_GITHUB_URL'
      );
    }
    owner = parts[0];
    repo = parts[1].replace(/\.git$/i, '');
    if (!subpath && parts.length > 2) {
      subpath = cleanSubpath(parts.slice(2).join('/'));
    }
  }

  return {
    owner,
    repo,
    ref,
    subpath,
    canonicalUrl: buildCanonicalGitHubUrl({ owner, repo, ref, subpath }),
  };
}

export function buildCanonicalGitHubUrl({ owner, repo, ref, subpath }) {
  const base = `https://github.com/${owner}/${repo}`;
  if (!subpath && !ref) return base;
  const r = ref || 'HEAD';
  return subpath ? `${base}/tree/${r}/${subpath}` : `${base}/tree/${r}`;
}

function cleanSubpath(p) {
  return String(p ?? '')
    .trim()
    .replace(/^\/+|\/+$/g, '')
    .replace(/\/{2,}/g, '/');
}

function githubHeaders(token) {
  const headers = {
    accept: 'application/vnd.github+json',
    'user-agent': 'SkillVault/0.1',
    'x-github-api-version': '2022-11-28',
  };
  if (token) {
    headers.authorization = `Bearer ${token}`;
  }
  return headers;
}

async function fetchGitHubJson(url, { token, fetchImpl }) {
  const res = await fetchImpl(url, { headers: githubHeaders(token) });
  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new HttpError(
      res.status === 404 ? 404 : 502,
      `GitHub API error (${res.status}): ${extractGitHubError(body)}`,
      'GITHUB_API_ERROR'
    );
  }
  return res.json();
}

function extractGitHubError(body) {
  try {
    const parsed = JSON.parse(body);
    return parsed.message || body.slice(0, 160);
  } catch {
    return body.slice(0, 160) || 'Request failed';
  }
}

/**
 * Check the latest commit SHA for a GitHub repository/subpath.
 */
export async function checkGitHubUpstream({
  owner,
  repo,
  ref = null,
  subpath = '',
  token = null,
  fetchImpl = globalThis.fetch,
}) {
  const qs = new URLSearchParams({ per_page: '1' });
  if (ref) qs.set('sha', ref);
  if (subpath) qs.set('path', subpath);

  const commits = await fetchGitHubJson(
    `https://api.github.com/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/commits?${qs}`,
    { token, fetchImpl }
  );

  if (!Array.isArray(commits) || commits.length === 0) {
    throw new HttpError(
      404,
      `No commits found for ${owner}/${repo}${subpath ? `/${subpath}` : ''}`,
      'GITHUB_PATH_NOT_FOUND'
    );
  }

  const latest = commits[0];
  const rawMessage = latest.commit?.message ?? '';
  return {
    latestRev: latest.sha,
    latestMessage: rawMessage.split(/\r?\n/)[0].slice(0, 200),
    latestAt: latest.commit?.committer?.date || latest.commit?.author?.date || isoNow(),
  };
}

/**
 * Fetch all files inside a GitHub skill directory and compute canonical file map + hash + metadata.
 */
export async function fetchGitHubSkillFiles({
  owner,
  repo,
  ref = null,
  subpath = '',
  token = null,
  fetchImpl = globalThis.fetch,
}) {
  const upstream = await checkGitHubUpstream({ owner, repo, ref, subpath, token, fetchImpl });
  const commitSha = upstream.latestRev;

  const treeData = await fetchGitHubJson(
    `https://api.github.com/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/git/trees/${commitSha}?recursive=1`,
    { token, fetchImpl }
  );

  const prefix = subpath ? `${cleanSubpath(subpath)}/` : '';
  const blobs = (treeData.tree ?? []).filter((item) => {
    if (item.type !== 'blob') return false;
    if (item.size && item.size > MAX_FILE_SIZE_BYTES) return false;
    if (prefix && !item.path.startsWith(prefix)) return false;
    const rel = prefix ? item.path.slice(prefix.length) : item.path;
    if (!rel) return false;
    const segments = rel.split('/');
    return !segments.some((seg) => IGNORED_SEGMENTS.has(seg));
  });

  if (blobs.length === 0) {
    throw new HttpError(
      404,
      `No files found at GitHub path "${subpath || '/'}"`,
      'GITHUB_EMPTY_SKILL'
    );
  }

  const rawFiles = {};
  // Fetch blobs in small concurrent batches to be friendly to Workers & GitHub
  const batchSize = 6;
  for (let i = 0; i < blobs.length; i += batchSize) {
    const slice = blobs.slice(i, i + batchSize);
    await Promise.all(
      slice.map(async (item) => {
        const rel = prefix ? item.path.slice(prefix.length) : item.path;
        const blob = await fetchGitHubJson(
          `https://api.github.com/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/git/blobs/${item.sha}`,
          { token, fetchImpl }
        );
        const decoded = decodeGitHubBlob(blob.content ?? '', blob.encoding ?? 'base64');
        rawFiles[rel] = {
          encoding: decoded.encoding,
          content: decoded.content,
          mode: item.mode === '100755' ? '0755' : '0644',
        };
      })
    );
  }

  const files = normalizeFileMap(rawFiles);
  const contentHash = await computeFilesHash(files);
  const skillMd = files['SKILL.md'];
  const meta =
    skillMd && skillMd.encoding === 'utf8'
      ? parseSkillFrontmatter(skillMd.content)
      : { name: '', description: '', version: '', raw: {} };

  return {
    rev: commitSha,
    message: upstream.latestMessage,
    checkedAt: isoNow(),
    files,
    contentHash,
    meta,
  };
}

function decodeGitHubBlob(content, encoding) {
  if (encoding !== 'base64') {
    return { encoding: 'utf8', content: String(content) };
  }
  const cleanB64 = String(content).replace(/\s+/g, '');
  const binaryStr = atob(cleanB64);
  const bytes = new Uint8Array(binaryStr.length);
  let hasNullByte = false;
  for (let i = 0; i < binaryStr.length; i++) {
    const code = binaryStr.charCodeAt(i);
    if (code === 0) hasNullByte = true;
    bytes[i] = code;
  }

  if (!hasNullByte) {
    try {
      const text = textDecoderFatal.decode(bytes);
      return { encoding: 'utf8', content: text };
    } catch {
      // Binary or non-UTF8 content falls back to base64
    }
  }
  return { encoding: 'base64', content: cleanB64 };
}
