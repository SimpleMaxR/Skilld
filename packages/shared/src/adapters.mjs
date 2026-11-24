export const DEFAULT_ADAPTERS = Object.freeze([
  {
    id: 'claude-code',
    name: 'Claude Code',
    skillsDir: '~/.claude/skills',
    configRoot: '~/.claude',
  },
  {
    id: 'codex',
    name: 'OpenAI Codex',
    skillsDir: '~/.codex/skills',
    configRoot: '~/.codex',
  },
  {
    id: 'codebuddy',
    name: 'CodeBuddy',
    skillsDir: '~/.codebuddy/skills',
    configRoot: '~/.codebuddy',
  },
  {
    id: 'cursor',
    name: 'Cursor',
    skillsDir: '~/.cursor/skills',
    configRoot: '~/.cursor',
  },
  {
    id: 'gemini-cli',
    name: 'Gemini CLI',
    skillsDir: '~/.gemini/skills',
    configRoot: '~/.gemini',
  },
  {
    id: 'workbuddy',
    name: 'WorkBuddy',
    skillsDir: '~/.workbuddy/skills',
    configRoot: '~/.workbuddy',
  },
  {
    id: 'agents-shared',
    name: 'Shared (.agents)',
    skillsDir: '~/.agents/skills',
    configRoot: '~/.agents',
  },
]);

export function getAdapterMap(adapters = DEFAULT_ADAPTERS) {
  const map = new Map();
  for (const adapter of adapters) {
    map.set(adapter.id, adapter);
  }
  return map;
}
