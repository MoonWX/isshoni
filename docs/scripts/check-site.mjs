#!/usr/bin/env node
// Checks the built project site for anything that would make a reader's browser contact another origin
// (docs/m1/06-deploy-and-ci.md §10.1, "06" below: local search, the theme's bundled fonts, no analytics, no
// external embeds). CI's site job and .github/workflows/site.yml run it after `vitepress build`.
//
//   node docs/scripts/check-site.mjs [DIST]     (DIST defaults to docs/.vitepress/dist)
//
// What it checks:
// - HTML: every attribute that loads something (src, srcset, href of <link>, data, poster, action, …) on every tag
//   but <a> and <area>, which only navigate when clicked, is a path on this site or a data: URL; so is every url()
//   and @import of a style attribute or <style> block, and a <meta http-equiv="refresh"> target.
// - CSS: every url() and @import, comments left out, is a path on this site or a data: URL.
// - JS, in .js and .mjs files and in every inline <script> of a page (where a `head` entry of the config would put an
//   analytics or tag-manager loader): no string names a host that serves fonts, scripts, analytics or search (the
//   list below). The theme carries one such fallback, a social link's icon from api.iconify.design for an icon given
//   by name whose CSS is missing; it is accepted only while no page has such an icon (the config gives its icon as
//   inline SVG).
// - The site's own config, inlined in every page: local search, and no Algolia settings.
//
// Exit 0 when the site passes, 1 with every finding listed, 2 on bad usage. A browser's network panel is the final
// check (06 S10); this one keeps a later change from bringing such a request back without anyone noticing.
import { readdir, readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const dist = path.resolve(process.argv[2] ?? path.join(here, '..', '.vitepress', 'dist'));

/** Hosts that serve fonts, scripts, analytics, search or icons. A JS string that names one is a finding. */
const THIRD_PARTY_HOSTS = [
  /(^|\.)fonts\.googleapis\.com$/,
  /(^|\.)fonts\.gstatic\.com$/,
  /(^|\.)google-analytics\.com$/,
  /(^|\.)googletagmanager\.com$/,
  /(^|\.)algolia\.net$/,
  /(^|\.)algolianet\.com$/,
  /(^|\.)algolia\.io$/,
  /(^|\.)cdn\.jsdelivr\.net$/,
  /(^|\.)unpkg\.com$/,
  /(^|\.)cdnjs\.cloudflare\.com$/,
  /(^|\.)esm\.sh$/,
  /(^|\.)plausible\.io$/,
  /(^|\.)api\.iconify\.design$/,
  /(^|\.)gravatar\.com$/,
];
const ICONIFY = /(^|\.)api\.iconify\.design$/;

/** Attributes that make the browser fetch their URL (on any tag but <a> and <area>). */
const LOADING_ATTRS = new Set(['src', 'srcset', 'href', 'data', 'poster', 'action', 'formaction', 'background']);

/** @type {string[]} */
const findings = [];
/** @param {string} file @param {string} what */
const report = (file, what) => findings.push(`${path.relative(dist, file)}: ${what}`);

/** A URL without the quotes and spaces around it. */
const unquote = (url) => url.trim().replace(/^['"]|['"]$/g, '');

/** Whether a URL leaves this site: a scheme other than data: or blob:, or a protocol-relative //host. */
function external(url) {
  const u = unquote(url);
  if (u === '' || /^(data|blob):/i.test(u)) return false;
  return /^[a-z][a-z0-9+.-]*:/i.test(u) || u.startsWith('//');
}

/** The url() and @import targets of a style sheet, comments left out, each once. */
function cssURLs(css) {
  const text = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const urls = new Set();
  for (const m of text.matchAll(/url\(\s*([^)]*?)\s*\)/gi)) urls.add(unquote(m[1]));
  for (const m of text.matchAll(/@import\s+(?:url\()?\s*(['"]?)([^'")\s;]+)\1/gi)) urls.add(unquote(m[2]));
  return [...urls];
}

/** Every file below dir. */
async function walk(dir) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...(await walk(p)));
    else out.push(p);
  }
  return out;
}

/** The attributes of one start tag, as [name, value] pairs (names in lower case). */
function attributes(tag) {
  const attrs = [];
  for (const m of tag.matchAll(/\s([^\s=/>"']+)(?:\s*=\s*("[^"]*"|'[^']*'|[^\s>]+))?/g)) {
    const raw = m[2] ?? '';
    const value = raw.startsWith('"') || raw.startsWith("'") ? raw.slice(1, -1) : raw;
    attrs.push([m[1].toLowerCase(), value.replaceAll('&amp;', '&')]);
  }
  return attrs;
}

/** @param {string} file @param {string} html */
function checkHTML(file, html) {
  const noComments = html.replace(/<!--[\s\S]*?-->/g, '');
  for (const m of noComments.matchAll(/<([a-zA-Z][a-zA-Z0-9-]*)(\s[^>]*)?>/g)) {
    const tag = m[1].toLowerCase();
    if (tag === 'a' || tag === 'area') continue;
    const attrs = attributes(m[2] ?? '');
    for (const [name, value] of attrs) {
      if (LOADING_ATTRS.has(name)) {
        const urls = name.endsWith('srcset') ? value.split(',').map((s) => s.trim().split(/\s+/)[0]) : [value];
        for (const url of urls) if (external(url)) report(file, `<${tag} ${name}="${url}"> loads from another origin`);
      }
      if (name === 'style') {
        for (const url of cssURLs(value)) if (external(url)) report(file, `<${tag} style> loads ${url}`);
      }
    }
    if (tag === 'meta' && attrs.some(([n, v]) => n === 'http-equiv' && v.toLowerCase() === 'refresh')) {
      const content = attrs.find(([n]) => n === 'content')?.[1] ?? '';
      const target = content.split(/;\s*url=/i)[1];
      if (target && external(target)) report(file, `<meta http-equiv="refresh"> goes to ${target}`);
    }
  }
  for (const m of noComments.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/gi)) {
    for (const url of cssURLs(m[1])) if (external(url)) report(file, `<style> loads ${url}`);
  }
}

/** @param {string} file @param {string} js @param {boolean} socialIconsByName */
function checkJS(file, js, socialIconsByName) {
  for (const m of js.matchAll(/(?:https?:)?\/\/([a-z0-9.-]+\.[a-z]{2,})/gi)) {
    const host = m[1].toLowerCase();
    if (!THIRD_PARTY_HOSTS.some((re) => re.test(host))) continue;
    if (ICONIFY.test(host) && !socialIconsByName) continue;
    report(file, `names ${host}, a host that serves fonts, scripts, analytics, search or icons`);
  }
}

/** The site config that VitePress inlines into a page, or null. @param {string} html */
function siteData(html) {
  const m = html.match(/__VP_SITE_DATA__\s*=\s*JSON\.parse\(("(?:[^"\\]|\\.)*")\)/);
  return m ? JSON.parse(JSON.parse(m[1])) : null;
}

async function main() {
  if (process.argv.length > 3) {
    console.error('usage: node docs/scripts/check-site.mjs [DIST]');
    process.exit(2);
  }
  if (!(await stat(dist).catch(() => null))?.isDirectory()) {
    console.error(`check-site: ${dist} is not a directory: run vitepress build first`);
    process.exit(2);
  }
  const files = await walk(dist);
  const html = files.filter((f) => f.endsWith('.html'));
  if (html.length === 0) {
    console.error(`check-site: no HTML in ${dist}`);
    process.exit(2);
  }

  /** @type {Map<string, string>} */
  const pages = new Map();
  let socialIconsByName = false;
  let config = null;
  for (const file of html) {
    const text = await readFile(file, 'utf8');
    pages.set(file, text);
    if (/class="vpi-social-/.test(text)) socialIconsByName = true;
    config ??= siteData(text);
  }
  for (const [file, text] of pages) {
    checkHTML(file, text);
    // Inline scripts (a <script src> is an attribute that checkHTML checks).
    for (const m of text.matchAll(/<script\b(?![^>]*\ssrc\s*=)[^>]*>([\s\S]*?)<\/script>/gi)) {
      checkJS(file, m[1], socialIconsByName);
    }
  }
  for (const file of files.filter((f) => f.endsWith('.css'))) {
    for (const url of cssURLs(await readFile(file, 'utf8'))) {
      if (external(url)) report(file, `loads ${url}`);
    }
  }
  for (const file of files.filter((f) => /\.m?js$/.test(f))) {
    checkJS(file, await readFile(file, 'utf8'), socialIconsByName);
  }

  if (!config) {
    findings.push(
      'no page carries the site config (__VP_SITE_DATA__): this check needs updating for the VitePress in use',
    );
  } else {
    const theme = config.themeConfig ?? {};
    if (theme.search?.provider !== 'local') {
      findings.push(`themeConfig.search.provider is ${JSON.stringify(theme.search?.provider)}, not "local"`);
    }
    if (theme.algolia || theme.search?.options?.appId) findings.push('the site config has Algolia settings');
  }

  if (findings.length > 0) {
    console.error(`check-site: ${findings.length} request(s) to other origins in ${dist} (06 §10.1):`);
    for (const f of findings) console.error(`  ${f}`);
    process.exit(1);
  }
  console.log(`check-site: ${html.length} pages and ${files.length} files load nothing from other origins`);
}

await main();
