import { checkSingleSkillUpstream, createApp } from './app.mjs';
import { createD1Store } from './store/d1-store.mjs';
import { WEB_ASSETS } from './web-assets.generated.mjs';

export default {
  async fetch(request, env) {
    const store = createD1Store(env.DB);
    const app = createApp({
      store,
      token: env.SKILLD_TOKEN,
      githubToken: env.GITHUB_TOKEN || null,
      webAssets: WEB_ASSETS,
      runtime: 'cloudflare-workers',
    });
    return app.fetch(request);
  },

  async scheduled(_event, env, ctx) {
    const store = createD1Store(env.DB);
    ctx.waitUntil(
      (async () => {
        const skills = await store.listSkills();
        for (const skill of skills) {
          if (!skill.upstreamUrl) continue;
          await checkSingleSkillUpstream({
            store,
            skill,
            githubToken: env.GITHUB_TOKEN || null,
            fetchImpl: globalThis.fetch,
          }).catch(() => {});
        }
      })()
    );
  },
};
