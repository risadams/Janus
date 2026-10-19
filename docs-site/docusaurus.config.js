// @ts-check
const { execSync } = require('child_process');
const { themes: prismThemes } = require('prism-react-renderer');
const repoLinks = require('./src/remark/repo-links');

// The version shown in the navbar is the newest release tag. A checkout
// without tags (a shallow clone) shows "dev".
function latestTag() {
  try {
    return execSync('git describe --tags --abbrev=0', { stdio: ['ignore', 'pipe', 'ignore'] })
      .toString()
      .trim();
  } catch {
    return 'dev';
  }
}
const version = latestTag();

// GitHub Pages serves the site at /Janus/, so a build uses that base. The
// development server (docusaurus start) serves at /, so the site is at
// http://localhost:3000/. BASE_URL overrides either.
const isDevServer = process.argv.includes('start');
const baseUrl = process.env.BASE_URL || (isDevServer ? '/' : '/Janus/');

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Janus',
  tagline:
    'A local proxy for Claude Code: see what every request costs, send chosen work to a local LM Studio model, keep secrets on your machine, and keep working when Claude is down.',
  favicon: 'img/favicon.svg',

  url: 'https://risadams.github.io',
  baseUrl,

  organizationName: 'risadams',
  projectName: 'Janus',
  trailingSlash: false,

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',
  markdown: {
    // Read .md files as CommonMark, not MDX: the docs are written for
    // GitHub, and their prose holds <id>, {…} and the like.
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  headTags: [
    // What a phone shows for the site saved to its home screen.
    {
      tagName: 'link',
      attributes: { rel: 'apple-touch-icon', href: `${baseUrl}img/apple-touch-icon.png` },
    },
    {
      tagName: 'link',
      attributes: {
        rel: 'stylesheet',
        type: 'text/css',
        href: 'https://fonts.googleapis.com/css2?family=Fraunces:ital,opsz,wght@0,9..144,400;0,9..144,500;0,9..144,600;1,9..144,400;1,9..144,500&family=Caveat:wght@400;600;700&family=JetBrains+Mono:wght@400;500&family=Crimson+Pro:ital,wght@0,400;0,500;0,600;1,400;1,500&display=swap',
      },
    },
  ],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: '../docs',
          exclude: ['adr/000-template.md'],
          sidebarPath: require.resolve('./sidebars.js'),
          editUrl: ({ docPath }) => `https://github.com/risadams/Janus/edit/main/docs/${docPath}`,
          beforeDefaultRemarkPlugins: [repoLinks],
        },
        blog: false,
        theme: {
          customCss: require.resolve('./src/css/custom.css'),
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      image: 'img/social-card.png',
      colorMode: {
        defaultMode: 'dark',
        disableSwitch: false,
        respectPrefersColorScheme: false,
      },
      navbar: {
        title: '',
        logo: {
          alt: 'Janus',
          src: 'img/logo.svg',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'mainSidebar',
            position: 'left',
            label: 'Docs',
          },
          {
            href: 'https://github.com/risadams/Janus/releases',
            label: 'Releases',
            position: 'left',
          },
          {
            type: 'html',
            position: 'right',
            value: `<span class="navbar-version-badge">${version}</span>`,
          },
          {
            href: 'https://github.com/sponsors/risadams',
            label: 'Sponsor',
            position: 'right',
            className: 'header-sponsor-link',
          },
          {
            href: 'https://github.com/risadams/Janus',
            position: 'right',
            className: 'header-github-link',
            'aria-label': 'GitHub repository',
          },
        ],
      },
      footer: {
        style: 'dark',
        links: [],
        copyright: `
          <div class="footer-bar">
            <div class="footer-bar__brand">
              <svg viewBox="10 16 180 168" width="20" height="20" aria-hidden="true">
                <path fill="#5ba3b8" fill-rule="evenodd" d="M94 20 A80 80 0 0 0 94 180 Z M36 80 a10 10 0 1 0 20 0 a10 10 0 1 0 -20 0 Z"></path>
                <path fill="#e24a8a" fill-rule="evenodd" d="M106 20 A80 80 0 0 1 106 180 Z M144 80 a10 10 0 1 0 20 0 a10 10 0 1 0 -20 0 Z"></path>
              </svg>
              <span><em>Janus</em>, for Claude Code</span>
            </div>
            <nav class="footer-bar__links" aria-label="Footer navigation">
              <a href="https://github.com/risadams/Janus">GitHub</a>
              <a href="https://github.com/risadams/Janus/releases">Releases</a>
              <a href="https://github.com/risadams/Janus/issues">Issues</a>
              <a href="https://github.com/risadams/Janus/security/policy">Security</a>
              <a href="https://github.com/sponsors/risadams">Sponsor</a>
            </nav>
            <div class="footer-bar__copy">© ${new Date().getFullYear()} Ris Adams · MIT licence · ${version}</div>
          </div>
        `,
      },
      prism: {
        theme: prismThemes.oneLight,
        darkTheme: prismThemes.gruvboxMaterialDark,
        additionalLanguages: ['bash', 'json', 'go', 'powershell', 'ini', 'diff'],
      },
    }),
};

module.exports = config;
