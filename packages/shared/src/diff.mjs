import { normalizeFileMap } from './hash.mjs';

const MAX_DIFF_LINES_PER_FILE = 1200;

/**
 * Compare two skill file maps (left = base/local, right = target/vault)
 * and produce per-file statuses and unified line-level diff hunks.
 */
export function diffFileMaps(leftInput, rightInput) {
  const left = leftInput ? normalizeFileMap(leftInput) : {};
  const right = rightInput ? normalizeFileMap(rightInput) : {};

  const allPaths = Array.from(new Set([...Object.keys(left), ...Object.keys(right)])).sort();
  const files = [];
  let changedCount = 0;

  for (const relPath of allPaths) {
    const l = left[relPath];
    const r = right[relPath];

    if (!l && r) {
      changedCount += 1;
      files.push(buildSingleSideDiff(relPath, 'added', r, '+'));
      continue;
    }

    if (l && !r) {
      changedCount += 1;
      files.push(buildSingleSideDiff(relPath, 'removed', l, '-'));
      continue;
    }

    if (l.encoding === r.encoding && l.mode === r.mode && l.content === r.content) {
      files.push({
        path: relPath,
        status: 'unchanged',
        binary: l.encoding === 'base64',
        modeChanged: false,
        leftMode: l.mode,
        rightMode: r.mode,
        hunks: [],
      });
      continue;
    }

    changedCount += 1;
    const isBinary = l.encoding === 'base64' || r.encoding === 'base64';
    const modeChanged = l.mode !== r.mode;

    if (isBinary) {
      files.push({
        path: relPath,
        status: 'modified',
        binary: true,
        modeChanged,
        leftMode: l.mode,
        rightMode: r.mode,
        hunks: [],
      });
      continue;
    }

    const hunks =
      l.content === r.content
        ? []
        : computeUnifiedHunks(splitLines(l.content), splitLines(r.content), 3);

    files.push({
      path: relPath,
      status: 'modified',
      binary: false,
      modeChanged,
      leftMode: l.mode,
      rightMode: r.mode,
      hunks,
    });
  }

  return {
    hasChanges: changedCount > 0,
    changedCount,
    files,
  };
}

function buildSingleSideDiff(relPath, status, entry, prefix) {
  if (entry.encoding === 'base64') {
    return {
      path: relPath,
      status,
      binary: true,
      modeChanged: false,
      leftMode: status === 'removed' ? entry.mode : null,
      rightMode: status === 'added' ? entry.mode : null,
      hunks: [],
    };
  }

  const lines = splitLines(entry.content);
  const capped = lines.slice(0, MAX_DIFF_LINES_PER_FILE);
  const diffLines = capped.map((text, idx) => ({
    type: prefix === '+' ? 'add' : 'del',
    leftLine: prefix === '-' ? idx + 1 : null,
    rightLine: prefix === '+' ? idx + 1 : null,
    text,
  }));

  if (lines.length > MAX_DIFF_LINES_PER_FILE) {
    diffLines.push({
      type: 'meta',
      leftLine: null,
      rightLine: null,
      text: `... truncated (${lines.length - MAX_DIFF_LINES_PER_FILE} more lines)`,
    });
  }

  return {
    path: relPath,
    status,
    binary: false,
    modeChanged: false,
    leftMode: status === 'removed' ? entry.mode : null,
    rightMode: status === 'added' ? entry.mode : null,
    hunks: [
      {
        header:
          status === 'added'
            ? `@@ -0,0 +1,${capped.length} @@`
            : `@@ -1,${capped.length} +0,0 @@`,
        lines: diffLines,
      },
    ],
  };
}

function splitLines(text) {
  if (text === '') return [];
  const normalized = text.replace(/\r\n/g, '\n');
  const lines = normalized.split('\n');
  if (lines.length > 0 && lines[lines.length - 1] === '') {
    lines.pop();
  }
  return lines;
}

/**
 * Compute unified diff hunks using Myers/LCS DP for typical skill files,
 * falling back to whole-block replacement for very large files.
 */
function computeUnifiedHunks(leftLines, rightLines, context = 3) {
  if (leftLines.length * rightLines.length > 400_000) {
    return [
      {
        header: `@@ -1,${leftLines.length} +1,${rightLines.length} @@`,
        lines: [
          ...leftLines.slice(0, 200).map((text, idx) => ({
            type: 'del',
            leftLine: idx + 1,
            rightLine: null,
            text,
          })),
          ...rightLines.slice(0, 200).map((text, idx) => ({
            type: 'add',
            leftLine: null,
            rightLine: idx + 1,
            text,
          })),
        ],
      },
    ];
  }

  const ops = computeLineDiffOps(leftLines, rightLines);
  const changeIndices = [];
  for (let i = 0; i < ops.length; i++) {
    if (ops[i].type !== 'ctx') changeIndices.push(i);
  }
  if (changeIndices.length === 0) return [];

  const hunks = [];
  let groupStart = Math.max(0, changeIndices[0] - context);
  let groupEnd = Math.min(ops.length - 1, changeIndices[0] + context);

  for (let k = 1; k < changeIndices.length; k++) {
    const idx = changeIndices[k];
    const start = Math.max(0, idx - context);
    const end = Math.min(ops.length - 1, idx + context);
    if (start <= groupEnd + 1) {
      groupEnd = end;
    } else {
      hunks.push(buildHunk(ops.slice(groupStart, groupEnd + 1)));
      groupStart = start;
      groupEnd = end;
    }
  }
  hunks.push(buildHunk(ops.slice(groupStart, groupEnd + 1)));
  return hunks;
}

function buildHunk(slice) {
  let leftStart = null;
  let rightStart = null;
  let leftCount = 0;
  let rightCount = 0;

  for (const op of slice) {
    if (op.leftLine !== null) {
      if (leftStart === null) leftStart = op.leftLine;
      leftCount += 1;
    }
    if (op.rightLine !== null) {
      if (rightStart === null) rightStart = op.rightLine;
      rightCount += 1;
    }
  }

  return {
    header: `@@ -${leftStart ?? 0},${leftCount} +${rightStart ?? 0},${rightCount} @@`,
    lines: slice,
  };
}

function computeLineDiffOps(a, b) {
  const n = a.length;
  const m = b.length;
  const width = m + 1;
  const dp = new Int32Array((n + 1) * (m + 1));

  for (let i = n - 1; i >= 0; i--) {
    const row = i * width;
    const nextRow = (i + 1) * width;
    for (let j = m - 1; j >= 0; j--) {
      if (a[i] === b[j]) {
        dp[row + j] = dp[nextRow + j + 1] + 1;
      } else {
        const down = dp[nextRow + j];
        const right = dp[row + j + 1];
        dp[row + j] = down >= right ? down : right;
      }
    }
  }

  const ops = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      ops.push({ type: 'ctx', leftLine: i + 1, rightLine: j + 1, text: a[i] });
      i += 1;
      j += 1;
    } else if (dp[(i + 1) * width + j] >= dp[i * width + j + 1]) {
      ops.push({ type: 'del', leftLine: i + 1, rightLine: null, text: a[i] });
      i += 1;
    } else {
      ops.push({ type: 'add', leftLine: null, rightLine: j + 1, text: b[j] });
      j += 1;
    }
  }
  while (i < n) {
    ops.push({ type: 'del', leftLine: i + 1, rightLine: null, text: a[i] });
    i += 1;
  }
  while (j < m) {
    ops.push({ type: 'add', leftLine: null, rightLine: j + 1, text: b[j] });
    j += 1;
  }
  return ops;
}
