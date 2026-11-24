import fs from 'node:fs/promises';
import path from 'node:path';
import { computeFilesHash } from '../packages/shared/src/index.mjs';

const serverUrl = (process.env.SKILLVAULT_URL || 'http://127.0.0.1:3737').replace(/\/+$/, '');
let token = process.env.SKILLVAULT_TOKEN || '';

if (!token) {
  try {
    token = (await fs.readFile(path.resolve('.data/token'), 'utf8')).trim();
  } catch {
    console.error('Set SKILLVAULT_TOKEN or start the local server first so .data/token exists.');
    process.exit(1);
  }
}

async function api(pathname, body) {
  const res = await fetch(`${serverUrl}${pathname}`, {
    method: body ? 'POST' : 'GET',
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${token}`,
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok && res.status !== 409) {
    throw new Error(`${pathname} failed (${res.status}): ${data.error || 'unknown'}`);
  }
  return data;
}

const reviewerV1 = {
  'SKILL.md': {
    encoding: 'utf8',
    mode: '0644',
    content: `---\nname: Code Reviewer\ndescription: Rigorous PR review checklist focusing on correctness and security\nversion: 1.0.0\n---\n\n# Code Reviewer\n\n1. Check error handling and edge cases.\n2. Verify input validation at boundaries.\n`,
  },
};

const reviewerV2 = {
  'SKILL.md': {
    encoding: 'utf8',
    mode: '0644',
    content: `---\nname: Code Reviewer\ndescription: Rigorous PR review checklist focusing on correctness, concurrency, and security\nversion: 1.1.0\n---\n\n# Code Reviewer\n\n1. Check error handling, resource cleanup, and edge cases.\n2. Verify input validation and auth checks at boundaries.\n3. Flag N+1 queries and unbounded loops.\n`,
  },
};

const reviewerDrifted = {
  'SKILL.md': {
    encoding: 'utf8',
    mode: '0644',
    content: `---\nname: Code Reviewer\ndescription: Rigorous PR review checklist focusing on correctness, concurrency, and security\nversion: 1.1.0\n---\n\n# Code Reviewer\n\n1. Check error handling, resource cleanup, and edge cases.\n2. Verify input validation and auth checks at boundaries.\n3. Flag N+1 queries and unbounded loops.\n4. Always check PostgreSQL migration locking safety.\n`,
  },
};

const sqlFiles = {
  'SKILL.md': {
    encoding: 'utf8',
    mode: '0644',
    content: `---\nname: SQL Optimizer\ndescription: Explain query plans and index recommendations\n---\n\n# SQL Optimizer\n\nInspect EXPLAIN ANALYZE before rewriting queries.\n`,
  },
  'scripts/check.sh': {
    encoding: 'utf8',
    mode: '0755',
    content: '#!/usr/bin/env bash\necho "Checking SQL plans..."\n',
  },
};

const localOnlySkill = {
  'SKILL.md': {
    encoding: 'utf8',
    mode: '0644',
    content: `---\nname: Docker Troubleshooter\ndescription: Diagnose container OOM and networking issues\n---\n\n# Docker Troubleshooter\n\nInspect container logs, inspect bridge networks, and check cgroup limits.\n`,
  },
};

const hashV1 = await computeFilesHash(reviewerV1);
const hashV2 = await computeFilesHash(reviewerV2);
const hashDrifted = await computeFilesHash(reviewerDrifted);
const hashSql = await computeFilesHash(sqlFiles);
const hashLocalOnly = await computeFilesHash(localOnlySkill);

// Create code-reviewer v1 then v2
const createdReviewer = await api('/api/skills', {
  id: 'code-reviewer',
  name: 'Code Reviewer',
  description: 'Rigorous PR review checklist',
  files: reviewerV1,
});
if (createdReviewer.skill) {
  await api('/api/skills/code-reviewer/versions', {
    files: reviewerV2,
    note: 'Add N+1 query check',
  });
}

await api('/api/skills', {
  id: 'sql-optimizer',
  name: 'SQL Optimizer',
  description: 'Explain query plans and index recommendations',
  files: sqlFiles,
});

// Report Mac device: claude-code synced at v2, codex drifted locally
await api('/api/devices/report', {
  device: {
    id: 'demo-macbook-pro',
    name: 'MacBook-Pro',
    os: 'darwin',
    arch: 'arm64',
    hostname: 'macbook-pro.local',
  },
  detectedAgents: [
    { id: 'claude-code', name: 'Claude Code', skillsDir: '~/.claude/skills', exists: true },
    { id: 'codex', name: 'OpenAI Codex', skillsDir: '~/.codex/skills', exists: true },
    { id: 'cursor', name: 'Cursor', skillsDir: '~/.cursor/skills', exists: true },
  ],
  scannedSkills: [
    {
      agentId: 'claude-code',
      skillName: 'code-reviewer',
      vaultSkillId: 'code-reviewer',
      baseHash: hashV2,
      actualHash: hashV2,
      meta: { name: 'Code Reviewer' },
    },
    {
      agentId: 'codex',
      skillName: 'code-reviewer',
      vaultSkillId: 'code-reviewer',
      baseHash: hashV2,
      actualHash: hashDrifted,
      meta: { name: 'Code Reviewer' },
      files: reviewerDrifted,
    },
    {
      agentId: 'claude-code',
      skillName: 'sql-optimizer',
      vaultSkillId: 'sql-optimizer',
      baseHash: hashSql,
      actualHash: hashSql,
      meta: { name: 'SQL Optimizer' },
    },
  ],
});

// Report Linux dev box: claude-code outdated (at v1), codebuddy has unmanaged local skill
await api('/api/devices/report', {
  device: {
    id: 'demo-linux-dev-01',
    name: 'linux-dev-01',
    os: 'linux',
    arch: 'x64',
    hostname: 'linux-dev-01',
  },
  detectedAgents: [
    { id: 'claude-code', name: 'Claude Code', skillsDir: '~/.claude/skills', exists: true },
    { id: 'codex', name: 'OpenAI Codex', skillsDir: '~/.codex/skills', exists: true },
    { id: 'codebuddy', name: 'CodeBuddy', skillsDir: '~/.codebuddy/skills', exists: true },
  ],
  scannedSkills: [
    {
      agentId: 'claude-code',
      skillName: 'code-reviewer',
      vaultSkillId: 'code-reviewer',
      baseHash: hashV1,
      actualHash: hashV1,
      meta: { name: 'Code Reviewer' },
    },
    {
      agentId: 'codebuddy',
      skillName: 'docker-troubleshooter',
      vaultSkillId: null,
      baseHash: null,
      actualHash: hashLocalOnly,
      meta: { name: 'Docker Troubleshooter' },
      files: localOnlySkill,
    },
  ],
});

console.log(`Seeded demo vault + 2 devices at ${serverUrl}`);
