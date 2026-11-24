/**
 * Lightweight zero-dependency YAML frontmatter parser tailored for SKILL.md.
 * Extracts top-level `name`, `description`, `version`, and raw metadata key-value pairs.
 */
export function parseSkillFrontmatter(markdownText) {
  const text = String(markdownText ?? '');
  const meta = {
    name: '',
    description: '',
    version: '',
    raw: {},
    body: text,
  };

  const match = text.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/);
  if (match) {
    const yamlBlock = match[1];
    meta.body = match[2] ?? '';
    const parsed = parseSimpleYaml(yamlBlock);
    meta.raw = parsed;
    if (typeof parsed.name === 'string') meta.name = parsed.name.trim();
    if (typeof parsed.description === 'string') meta.description = parsed.description.trim();
    if (parsed.version !== undefined && parsed.version !== null) {
      meta.version = String(parsed.version).trim();
    }
  }

  if (!meta.name) {
    const headingMatch = meta.body.match(/^\s*#\s+(.+?)\s*$/m);
    if (headingMatch) {
      meta.name = headingMatch[1].trim();
    }
  }

  return meta;
}

function parseSimpleYaml(yamlBlock) {
  const result = {};
  const lines = yamlBlock.split(/\r?\n/);
  let currentKey = null;
  let blockMode = null; // '|' or '>' or 'list'
  let blockLines = [];

  const flushBlock = () => {
    if (!currentKey) return;
    if (blockMode === 'list') {
      result[currentKey] = blockLines.slice();
    } else if (blockMode === '|') {
      result[currentKey] = blockLines.join('\n').trimEnd();
    } else if (blockMode === '>') {
      result[currentKey] = blockLines.join(' ').trim();
    }
    currentKey = null;
    blockMode = null;
    blockLines = [];
  };

  for (const line of lines) {
    if (!line.trim() && !blockMode) continue;
    if (line.trim().startsWith('#') && !blockMode) continue;

    if (blockMode && (/^[ \t]+/.test(line) || line.trim() === '')) {
      if (blockMode === 'list') {
        const itemMatch = line.match(/^\s*-\s+(.*)$/);
        if (itemMatch) blockLines.push(unquoteYamlScalar(itemMatch[1]));
      } else {
        blockLines.push(line.replace(/^[ \t]{2}/, ''));
      }
      continue;
    }

    if (blockMode && /^\s*-\s+/.test(line) && currentKey) {
      blockMode = 'list';
      const itemMatch = line.match(/^\s*-\s+(.*)$/);
      if (itemMatch) blockLines.push(unquoteYamlScalar(itemMatch[1]));
      continue;
    }

    flushBlock();

    const kvMatch = line.match(/^([A-Za-z0-9_.-]+)\s*:\s*(.*)$/);
    if (!kvMatch) continue;

    const key = kvMatch[1].trim();
    const rawVal = kvMatch[2].trim();

    if (rawVal === '|' || rawVal === '|-' || rawVal === '>' || rawVal === '>-') {
      currentKey = key;
      blockMode = rawVal.startsWith('|') ? '|' : '>';
      blockLines = [];
    } else if (rawVal === '') {
      currentKey = key;
      blockMode = 'list';
      blockLines = [];
    } else {
      result[key] = unquoteYamlScalar(rawVal);
    }
  }

  flushBlock();
  return result;
}

function unquoteYamlScalar(val) {
  const trimmed = String(val ?? '').trim();
  if (
    (trimmed.startsWith('"') && trimmed.endsWith('"') && trimmed.length >= 2) ||
    (trimmed.startsWith("'") && trimmed.endsWith("'") && trimmed.length >= 2)
  ) {
    return trimmed.slice(1, -1);
  }
  return trimmed;
}
