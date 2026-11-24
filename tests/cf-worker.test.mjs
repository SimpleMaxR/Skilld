import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import test from 'node:test';
import workerEntry from '../packages/server/src/entry-worker.mjs';

function createMockD1Database() {
  const db = new DatabaseSync(':memory:');
  db.exec('PRAGMA foreign_keys = ON;');

  function makeStatement(sql, bound = []) {
    return {
      bind(...params) {
        return makeStatement(sql, params);
      },
      async all() {
        return { success: true, results: db.prepare(sql).all(...bound) };
      },
      async first() {
        return db.prepare(sql).get(...bound) ?? null;
      },
      async run() {
        const res = db.prepare(sql).run(...bound);
        return { success: true, meta: { changes: res.changes } };
      },
    };
  }

  return {
    prepare: (sql) => makeStatement(sql),
    async batch(statements) {
      db.exec('BEGIN');
      try {
        const out = [];
        for (const stmt of statements) out.push(await stmt.run());
        db.exec('COMMIT');
        return out;
      } catch (err) {
        db.exec('ROLLBACK');
        throw err;
      }
    },
    exec: (sql) => db.exec(sql),
    close: () => db.close(),
  };
}

test('Optional Cloudflare Worker + D1 entrypoint serves health, auth, and embedded web UI', async () => {
  const schemaSql = await fs.readFile(path.resolve('packages/server/schema.sql'), 'utf8');
  const d1 = createMockD1Database();
  d1.exec(schemaSql);

  try {
    const env = { DB: d1, SKILLD_TOKEN: 'cf-optional-token' };

    const healthRes = await workerEntry.fetch(new Request('https://worker.local/api/health'), env);
    assert.equal(healthRes.status, 200);
    const healthJson = await healthRes.json();
    assert.equal(healthJson.runtime, 'cloudflare-workers');

    const unauth = await workerEntry.fetch(new Request('https://worker.local/api/overview'), env);
    assert.equal(unauth.status, 401);

    const overviewRes = await workerEntry.fetch(
      new Request('https://worker.local/api/overview', {
        headers: { authorization: 'Bearer cf-optional-token' },
      }),
      env
    );
    assert.equal(overviewRes.status, 200);

    const uiRes = await workerEntry.fetch(new Request('https://worker.local/'), env);
    assert.equal(uiRes.status, 200);
    assert.match(await uiRes.text(), /Skilld/);
  } finally {
    d1.close();
  }
});
