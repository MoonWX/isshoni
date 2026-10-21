// The project site (docs/m1/06-deploy-and-ci.md §10, "06" below): VitePress over docs/, published to GitHub Pages
// at https://moonwx.github.io/isshoni/ by .github/workflows/site.yml.
//
// - The design documents stay on GitHub: docs/PLAN.md, docs/m1/ and docs/m2/ are not pages of the site (srcExclude).
// - No request leaves for another origin (06 §10.1): local search, the default theme's bundled Inter font (never
//   Google Fonts), no analytics, no cookies, no embeds. scripts/check-site.mjs checks the built site for it.
// - The server, the installer and the web client link to fixed anchors here (06 §10.4): /troubleshooting#doctor-<id>,
//   /troubleshooting#ct-<code>, /install/vps#<provider>. TestDocsAnchors (internal/server/ops/doctor) fails when one
//   is missing, so those headings carry explicit {#anchors} that a reworded title can't move.
// - release.data.ts reads the latest release's facts, which site.yml writes into .vitepress/release.json.
import { defineConfig } from 'vitepress';

const repo = 'https://github.com/MoonWX/isshoni';
const base = '/isshoni/';

// GitHub's mark (Simple Icons, CC0-1.0) inline. A social link given by name gets its icon from a generated CSS file,
// and the theme falls back to loading it from api.iconify.design when that CSS is missing: inline, nothing can.
const githubIcon =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M12 .297c-6.63 0-12 5.373-12 12c0 5.303 3.438 9.8 8.205 11.385c.6.113.82-.258.82-.577c0-.285-.01-1.04-.015-2.04c-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729c1.205.084 1.838 1.236 1.838 1.236c1.07 1.835 2.809 1.305 3.495.998c.108-.776.417-1.305.76-1.605c-2.665-.3-5.466-1.332-5.466-5.93c0-1.31.465-2.38 1.235-3.22c-.135-.303-.54-1.523.105-3.176c0 0 1.005-.322 3.3 1.23c.96-.267 1.98-.399 3-.405c1.02.006 2.04.138 3 .405c2.28-1.552 3.285-1.23 3.285-1.23c.645 1.653.24 2.873.12 3.176c.765.84 1.23 1.91 1.23 3.22c0 4.61-2.805 5.625-5.475 5.92c.42.36.81 1.096.81 2.22c0 1.606-.015 2.896-.015 3.286c0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12"/></svg>';

export default defineConfig({
  lang: 'en-US',
  title: 'isshoni',
  description: 'Self-hosted screen sharing with system audio for a group of friends.',
  base,
  // /install/docker, not /install/docker.html. GitHub Pages serves docker.html for both.
  cleanUrls: true,
  // The design documents (06 D11, §17) and this site's own tooling are no pages.
  srcExclude: ['PLAN.md', 'm1/**', 'm2/**', 'scripts/**'],
  // The default theme's Inter comes from its own files; the Google Fonts import is never used (06 §10.1).
  useWebFonts: false,
  // Last-updated dates would need the git history of every page in CI, and say little on a small site.
  lastUpdated: false,
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: `${base}icon.svg` }],
    ['meta', { name: 'theme-color', content: '#101014' }],
    // Nobody's address leaves for a referrer header when a reader follows a link to another site.
    ['meta', { name: 'referrer', content: 'no-referrer' }],
  ],
  themeConfig: {
    logo: { src: '/icon.svg', alt: '' },
    nav: [
      { text: 'Install', link: '/install/', activeMatch: '^/install/' },
      { text: 'Guide for friends', link: '/guide/' },
      { text: 'Troubleshooting', link: '/troubleshooting' },
      { text: 'Reference', link: '/reference/config', activeMatch: '^/reference/' },
    ],
    sidebar: [
      {
        text: 'Install',
        items: [
          { text: 'Install script', link: '/install/' },
          { text: 'Docker', link: '/install/docker' },
          { text: 'Choosing a VPS and its firewall', link: '/install/vps' },
          { text: 'Behind a reverse proxy', link: '/install/reverse-proxy' },
          { text: 'Certificates (TLS modes)', link: '/install/tls' },
        ],
      },
      {
        text: 'Use',
        items: [
          { text: 'Guide for friends', link: '/guide/' },
          { text: 'Troubleshooting', link: '/troubleshooting' },
          { text: 'Desktop apps', link: '/download' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Configuration', link: '/reference/config' },
          { text: 'Command line', link: '/reference/cli' },
        ],
      },
      {
        text: 'Project',
        items: [
          { text: 'Security and verifying downloads', link: '/security' },
          { text: 'Privacy', link: '/privacy' },
          { text: 'Code signing policy', link: '/code-signing' },
        ],
      },
    ],
    outline: { level: [2, 3] },
    // Local search: the index is built with the site and searched in the browser (06 §10.1; no Algolia).
    search: { provider: 'local' },
    socialLinks: [{ icon: { svg: githubIcon }, link: repo, ariaLabel: 'isshoni on GitHub' }],
    editLink: {
      pattern: `${repo}/edit/main/docs/:path`,
      text: 'Edit this page on GitHub',
    },
    footer: {
      message: `Apache License 2.0. Hosted on GitHub Pages; see <a href="${base}privacy">Privacy</a>.`,
    },
  },
});
