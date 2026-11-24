import assert from 'node:assert/strict';
import test from 'node:test';
import { DatabaseSync } from 'node:sqlite';
import { createApp } from '../packages/server/src/app.mjs';
import { createD1Store } from '../packages/server/src/store/d1-store.mjs';
import { parseGitHubUrl } from '../packages/server/src/upstream.mjs';

function createMockD1() {
  const db = new DatabaseSync(':memory:');
  db.exec('PRAGMA foreign_keys = ON;');
  const makeStmt = (sql, bound = []) => ({
    bind: (...params) => makeStmt(sql, params),
    all: async () => ({ success: true, results: db.prepare(sql).all(...bound) }),
    first: async () => db.prepare(sql).get(...bound) ?? null,
    run: async () => ({ success: true, meta: { changes: db.prepare(sql).run(...bound).changes } }),
  });
  return {
    prepare: (sql) => makeStmt(sql),
    batch: async (stmts) => {
      db.exec('BEGIN');
      try {
        const out = [];
        for (const s of stmts) out.push(await s.run());
        db.exec('COMMIT');
        return out;
      } catch (err) {
        db.exec('ROLLBACK');
        throw err;
      }
    },
    close: () => db.close(),
  };
}

test('parseGitHubUrl handles standard URLs, tree subpaths, blob SKILL.md links, and shorthands', () => {
  const tree = parseGitHubUrl('https://github.com/anthropics/skills/tree/main/skills/pdf');
  assert.deepEqual(tree, {
    owner: 'anthropics',
    repo: 'skills',
    ref: 'main',
    subpath: 'skills/pdf',
    canonicalUrl: 'https://github.com/anthropics/skills/tree/main/skills/pdf',
  });

  const blob = parseGitHubUrl('https://github.com/acme/repo/blob/v2/my-skill/SKILL.md');
  assert.equal(blob.ref, 'v2');
  assert.equal(blob.subpath, 'my-skill');

  const short = parseGitHubUrl('acme/my-skills/docker-debug');
  assert.equal(short.owner, 'acme');
  assert.equal(short.repo, 'my-skills');
  assert.equal(short.subpath, 'docker-debug');

  assert.throws(() => parseGitHubUrl('https://gitlab.com/a/b'), /Only github.com/);
});

test('GitHub import, upstream check, and upgrade work on optional Cloudflare D1 store', async () => {
  const d1 = createMockD1();
  const store = createD1Store(d1);
  await store.ensureSchema();

  let revState = 'v1';

  const v1SkillMd = Buffer.from(
    '---\nname: GitHub PDF Skill\ndescription: Upstream PDF skill v1\n---\n# PDF v1\n',
    'utf8'
  ).toString('base64');
  const v2SkillMd = Buffer.from(
    '---\nname: GitHub PDF Skill\ndescription: Upstream PDF skill v2\n---\n# PDF v2\nNew step.\n',
    'utf8'
  ).toString('base64');
  const scriptB64 = Buffer.from('#!/usr/bin/env bash\necho ok\n', 'utf8').toString('base64');

  const mockFetch = async (url) => {
    const u = String(url);
    if (u.includes('/commits?')) {
      const commit =
        revState === 'v1'
          ? {
              sha: '1111111111111111111111111111111111111111',
              commit: { message: 'Initial skill\n\nDetails', committer: { date: '2026-10-01T00:00:00Z' } },
            }
          : {
              sha: '2222222222222222222222222222222222222222',
              commit: { message: 'Improve PDF parsing', committer: { date: '2026-11-01T00:00:00Z' } },
            };
      return new Response(JSON.stringify([commit]), { status: 200 });
    }

    if (u.includes('/git/trees/1111')) {
      return new Response(
        JSON.stringify({
          tree: [
            { path: 'skills/pdf/SKILL.md', type: 'blob', mode: '100644', sha: 'blob_md_v1', size: 80 },
            { path: 'skills/pdf/scripts/run.sh', type: 'blob', mode: '100755', sha: 'blob_sh', size: 30 },
            { path: 'skills/other/SKILL.md', type: 'blob', mode: '100644', sha: 'ignored', size: 20 },
          ],
        }),
        { status: 200 }
      );
    }

    if (u.includes('/git/trees/2222')) {
      return new Response(
        JSON.stringify({
          tree: [
            { path: 'skills/pdf/SKILL.md', type: 'blob', mode: '100644', sha: 'blob_md_v2', size: 92 },
            { path: 'skills/pdf/scripts/run.sh', type: 'blob', mode: '100755', sha: 'blob_sh', size: 30 },
          ],
        }),
        { status: 200 }
      );
    }

    if (u.endsWith('/git/blobs/blob_md_v1')) {
      return new Response(JSON.stringify({ encoding: 'base64', content: v1SkillMd }), { status: 200 });
    }
    if (u.endsWith('/git/blobs/blob_md_v2')) {
      return new Response(JSON.stringify({ encoding: 'base64', content: v2SkillMd }), { status: 200 });
    }
    if (u.endsWith('/git/blobs/blob_sh')) {
      return new Response(JSON.stringify({ encoding: 'base64', content: scriptB64 }), { status: 200 });
    }

    return new Response(JSON.stringify({ message: 'Not Found' }), { status: 404 });
  };

  try {
    const token = 'upstream-test-token';
    const app = createApp({ store, token, fetchImpl: mockFetch });

    const callApi = async (pathname, body) => {
      const res = await app.fetch(
        new Request(`http://skilld.local${pathname}`, {
          method: 'POST',
          headers: {
            'content-type': 'application/json',
            authorization: `Bearer ${token}`,
          },
          body: JSON.stringify(body),
        })
      );
      return { status: res.status, json: await res.json() };
    };

    const imported = await callApi('/api/skills/import-github', {
      url: 'https://github.com/anthropics/skills/tree/main/skills/pdf',
    });
    assert.equal(imported.status, 201);
    const skillId = imported.json.skill.id;
    assert.equal(imported.json.skill.name, 'GitHub PDF Skill');
    assert.equal(imported.json.skill.hasUpstreamUpdate, false);
    assert.equal(imported.json.skill.currentVersion.files['scripts/run.sh'].mode, '0755');

    revState = 'v2';
    const checked = await callApi('/api/skills/check-all-upstream', {});
    assert.equal(checked.status, 200);
    assert.equal(checked.json.results[0].hasUpstreamUpdate, true);

    const upgraded = await callApi(`/api/skills/${skillId}/upgrade-upstream`, {});
    assert.equal(upgraded.status, 200);
    assert.equal(upgraded.json.upgraded, true);
    assert.equal(upgraded.json.version.versionNum, 2);
    assert.equal(upgraded.json.skill.hasUpstreamUpdate, false);
    assert.match(upgraded.json.skill.currentVersion.files['SKILL.md'].content, /PDF v2/);
  } finally {
    d1.close();
  }
});
