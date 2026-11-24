import assert from 'node:assert/strict';
import test from 'node:test';
import {
  INSTALL_STATUS,
  computeFilesHash,
  computeInstallationStatus,
  diffFileMaps,
  isValidRelativePath,
  normalizeFileMap,
  parseSkillFrontmatter,
  slugifySkillId,
} from '../packages/shared/src/index.mjs';

test('slugifySkillId and isValidRelativePath handle edge cases safely', () => {
  assert.equal(slugifySkillId('Code Reviewer (v2)!'), 'code-reviewer-v2');
  assert.equal(slugifySkillId('   '), 'skill');

  assert.equal(isValidRelativePath('SKILL.md'), true);
  assert.equal(isValidRelativePath('scripts/run.sh'), true);
  assert.equal(isValidRelativePath('../escape.txt'), false);
  assert.equal(isValidRelativePath('sub/../escape.txt'), false);
  assert.equal(isValidRelativePath('/abs/path.md'), false);
  assert.equal(isValidRelativePath('win\\path.md'), false);
  assert.equal(isValidRelativePath('dir/'), false);
  assert.equal(isValidRelativePath(''), false);
});

test('computeFilesHash is deterministic across key order and sensitive to content and mode', async () => {
  const mapA = {
    'scripts/run.sh': { encoding: 'utf8', content: 'echo hi\n', mode: '0755' },
    'SKILL.md': { encoding: 'utf8', content: '# Test\n', mode: '0644' },
  };
  const mapB = {
    'SKILL.md': { encoding: 'utf8', content: '# Test\n', mode: '0644' },
    'scripts/run.sh': { encoding: 'utf8', content: 'echo hi\n', mode: '0755' },
  };
  const mapModeChanged = {
    'SKILL.md': { encoding: 'utf8', content: '# Test\n', mode: '0644' },
    'scripts/run.sh': { encoding: 'utf8', content: 'echo hi\n', mode: '0644' },
  };
  const mapContentChanged = {
    'SKILL.md': { encoding: 'utf8', content: '# Test!\n', mode: '0644' },
    'scripts/run.sh': { encoding: 'utf8', content: 'echo hi\n', mode: '0755' },
  };

  const hashA = await computeFilesHash(mapA);
  const hashB = await computeFilesHash(mapB);
  const hashMode = await computeFilesHash(mapModeChanged);
  const hashContent = await computeFilesHash(mapContentChanged);

  assert.match(hashA, /^sha256:[0-9a-f]{64}$/);
  assert.equal(hashA, hashB);
  assert.notEqual(hashA, hashMode);
  assert.notEqual(hashA, hashContent);

  assert.throws(() => normalizeFileMap({ '../bad': 'x' }), /Invalid relative skill file path/);
});

test('parseSkillFrontmatter extracts YAML metadata and falls back to heading', () => {
  const withYaml = `---
name: "PDF Specialist"
description: Extract tables and text from PDFs
version: 1.2.3
allowed-tools:
  - Read
  - Bash
---
# Ignored Heading
Body text.
`;
  const parsed = parseSkillFrontmatter(withYaml);
  assert.equal(parsed.name, 'PDF Specialist');
  assert.equal(parsed.description, 'Extract tables and text from PDFs');
  assert.equal(parsed.version, '1.2.3');
  assert.deepEqual(parsed.raw['allowed-tools'], ['Read', 'Bash']);

  const withoutYaml = `# Heading Fallback Skill\n\nSome instructions.`;
  const fallback = parseSkillFrontmatter(withoutYaml);
  assert.equal(fallback.name, 'Heading Fallback Skill');
});

test('computeInstallationStatus covers all 5 states cleanly', () => {
  const h1 = 'sha256:111';
  const h2 = 'sha256:222';
  const h3 = 'sha256:333';

  assert.equal(
    computeInstallationStatus({ baseHash: h1, actualHash: h1, desiredHash: h1, isManaged: true }),
    INSTALL_STATUS.SYNCED
  );
  assert.equal(
    computeInstallationStatus({ baseHash: h1, actualHash: h1, desiredHash: h2, isManaged: true }),
    INSTALL_STATUS.OUTDATED
  );
  assert.equal(
    computeInstallationStatus({ baseHash: h1, actualHash: h3, desiredHash: h1, isManaged: true }),
    INSTALL_STATUS.MODIFIED
  );
  assert.equal(
    computeInstallationStatus({ baseHash: h1, actualHash: h3, desiredHash: h2, isManaged: true }),
    INSTALL_STATUS.MODIFIED
  );
  assert.equal(
    computeInstallationStatus({ baseHash: null, actualHash: h1, desiredHash: null, isManaged: false }),
    INSTALL_STATUS.UNMANAGED
  );
  assert.equal(
    computeInstallationStatus({ baseHash: h1, actualHash: null, desiredHash: h1, isManaged: true }),
    INSTALL_STATUS.MISSING
  );
});

test('diffFileMaps produces unified hunks for modified, added, and removed files', () => {
  const left = {
    'SKILL.md': { encoding: 'utf8', content: 'line 1\nline 2\nline 3\n', mode: '0644' },
    'old.txt': { encoding: 'utf8', content: 'deprecated\n', mode: '0644' },
  };
  const right = {
    'SKILL.md': { encoding: 'utf8', content: 'line 1\nline 2 edited\nline 3\nline 4\n', mode: '0644' },
    'new.sh': { encoding: 'utf8', content: '#!/bin/sh\necho ok\n', mode: '0755' },
  };

  const diff = diffFileMaps(left, right);
  assert.equal(diff.hasChanges, true);
  assert.equal(diff.changedCount, 3);

  const skillDiff = diff.files.find((f) => f.path === 'SKILL.md');
  assert.equal(skillDiff.status, 'modified');
  assert.equal(skillDiff.hunks.length, 1);
  const lines = skillDiff.hunks[0].lines;
  assert.ok(lines.some((l) => l.type === 'del' && l.text === 'line 2'));
  assert.ok(lines.some((l) => l.type === 'add' && l.text === 'line 2 edited'));
  assert.ok(lines.some((l) => l.type === 'add' && l.text === 'line 4'));

  assert.equal(diff.files.find((f) => f.path === 'old.txt').status, 'removed');
  assert.equal(diff.files.find((f) => f.path === 'new.sh').status, 'added');
});
