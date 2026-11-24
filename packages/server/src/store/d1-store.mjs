import { createSqlStore, normalizeParams } from './shared-store.mjs';

/**
 * Create a SkillVault store backed by Cloudflare Workers D1Database.
 */
export function createD1Store(d1Database) {
  const adapter = {
    async all(sql, params = []) {
      const res = await d1Database.prepare(sql).bind(...normalizeParams(params)).all();
      return res?.results ?? [];
    },
    async first(sql, params = []) {
      const row = await d1Database.prepare(sql).bind(...normalizeParams(params)).first();
      return row ?? null;
    },
    async run(sql, params = []) {
      return d1Database.prepare(sql).bind(...normalizeParams(params)).run();
    },
    async batch(statements) {
      const prepared = statements.map(({ sql, params = [] }) =>
        d1Database.prepare(sql).bind(...normalizeParams(params))
      );
      return d1Database.batch(prepared);
    },
  };

  return createSqlStore(adapter);
}
