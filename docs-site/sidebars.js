// @ts-check

// The docs in the order a newcomer reads them: get it running, learn it,
// try a recipe, then the reference. The project's own papers (charter,
// research, decisions) come last, collapsed.
//
// Every page is listed by hand, under a short label, so the order is the
// one written here and never a file name's. Research notes are grouped by
// what they are about, each group in the order its questions were asked
// (by issue), and decisions by ADR number. scripts/check-sidebar.js fails
// the build if a page under docs/ is missing from this file.

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  mainSidebar: [
    { type: 'doc', id: 'QUICKSTART', label: 'Quick start' },
    {
      type: 'category',
      label: 'User guide',
      link: { type: 'doc', id: 'guide/README' },
      items: [
        { type: 'doc', id: 'guide/before-you-start', label: 'Before you start' },
        { type: 'doc', id: 'guide/install', label: 'Installing' },
        { type: 'doc', id: 'guide/configure', label: 'Configuring' },
        { type: 'doc', id: 'guide/use', label: 'Using' },
        { type: 'doc', id: 'TROUBLESHOOTING', label: 'Troubleshooting' },
      ],
    },
    {
      type: 'category',
      label: 'Recipes',
      items: [
        { type: 'doc', id: 'recipes/claude-code', label: 'Subagents on a local model' },
        { type: 'doc', id: 'recipes/local-first', label: 'Local first, Claude as the net' },
        { type: 'doc', id: 'recipes/outage', label: 'Keep working when Claude is down' },
        { type: 'doc', id: 'recipes/providers', label: 'Choose providers and their order' },
        { type: 'doc', id: 'recipes/pace', label: 'Move work local before a limit' },
        { type: 'doc', id: 'recipes/privacy', label: 'Keep secrets and files in' },
        { type: 'doc', id: 'recipes/workers', label: 'Hand reading to the local model' },
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        { type: 'doc', id: 'CONFIG', label: 'Configuration' },
        { type: 'doc', id: 'LEDGER', label: 'The ledger and captures' },
        { type: 'doc', id: 'GUARD', label: 'The guard' },
        { type: 'doc', id: 'WHAT-JANUS-CHANGES', label: 'What Janus changes' },
        { type: 'doc', id: 'THREAT-MODEL', label: 'Threat model' },
      ],
    },
    {
      type: 'category',
      label: 'The project',
      collapsed: true,
      items: [
        { type: 'doc', id: 'CHARTER', label: 'Charter' },
        { type: 'doc', id: 'REQUIREMENTS', label: 'Requirements' },
        { type: 'doc', id: 'ROADMAP', label: 'Roadmap' },
        { type: 'doc', id: 'RELEASES', label: 'Releases' },
        {
          type: 'category',
          label: 'Research',
          collapsed: true,
          items: [
            { type: 'doc', id: 'research/results-1.0', label: 'How the charter\'s goals came out' },
            {
              type: 'category',
              label: 'Claude Code\'s traffic',
              items: [
                { type: 'doc', id: 'research/traffic', label: 'What Claude Code sends' },
                { type: 'doc', id: 'research/integrity', label: 'Mixing local and upstream turns' },
                { type: 'doc', id: 'research/baseline', label: 'Where the cost goes' },
                { type: 'doc', id: 'research/keepwarm', label: 'Keeping the cache warm' },
              ],
            },
            {
              type: 'category',
              label: 'Local models',
              items: [
                { type: 'doc', id: 'research/lmstudio', label: 'LM Studio\'s API' },
                { type: 'doc', id: 'research/models', label: 'Which models do which jobs' },
                { type: 'doc', id: 'research/delegation', label: 'Does delegating a read pay?' },
                { type: 'doc', id: 'research/standin', label: 'The stand-in at a limit' },
              ],
            },
            {
              type: 'category',
              label: 'Saving context',
              items: [
                { type: 'doc', id: 'research/pruning', label: 'Pruning superseded context' },
                { type: 'doc', id: 'research/digest', label: 'Digesting large tool results' },
              ],
            },
            {
              type: 'category',
              label: 'OpenAI and the Codex CLI',
              items: [
                { type: 'doc', id: 'research/openai', label: 'Claude Code on the Responses API' },
                { type: 'doc', id: 'research/codex-cli', label: 'What the Codex CLI sends' },
              ],
            },
            {
              type: 'category',
              label: 'Janus itself',
              items: [
                { type: 'doc', id: 'research/performance', label: 'What each feature costs in speed' },
                { type: 'doc', id: 'research/loopback-stall', label: 'Large bodies stalling in CI' },
              ],
            },
          ],
        },
        {
          type: 'category',
          label: 'Design decisions',
          collapsed: true,
          items: [
            { type: 'doc', id: 'adr/dependencies', label: 'ADR-001 Dependencies' },
            { type: 'doc', id: 'adr/docs-site', label: 'ADR-002 The docs site' },
            { type: 'doc', id: 'adr/signed-checksums', label: 'ADR-003 Signed checksums' },
            { type: 'doc', id: 'adr/release-tooling', label: 'ADR-004 Release tooling' },
            { type: 'doc', id: 'adr/openai-credential', label: 'ADR-005 The OpenAI credential' },
          ],
        },
        { type: 'doc', id: 'diagrams/README', label: 'Diagrams' },
      ],
    },
  ],
};

module.exports = sidebars;
