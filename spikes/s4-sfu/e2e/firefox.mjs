// Cross-browser check: Chrome (Playwright) shares the canvas test pattern with a
// given H.264 profile; the installed Firefox (Puppeteer/WebDriver BiDi, headless)
// watches and runs the auto-switch test. Then Firefox shares and Chrome watches.
import { chromium } from 'playwright-core';
import puppeteer from 'puppeteer-core';

const BASE = process.env.BASE || 'http://localhost:8080';
const PROFILES = (process.env.PROFILES || 'auto,42e01f,64001f').split(',');
const SWITCHES = Number(process.env.SWITCHES || 6);
const FIREFOX = process.env.FIREFOX || '/Applications/Firefox.app/Contents/MacOS/firefox';

const chrome = await chromium.launch({
  channel: 'chrome', headless: true,
  args: ['--autoplay-policy=no-user-gesture-required', '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'],
});
const firefox = await puppeteer.launch({
  browser: 'firefox', executablePath: FIREFOX, headless: true,
  extraPrefsFirefox: {
    'media.autoplay.default': 0, 'media.autoplay.blocking_policy': 0,
    'media.navigator.permission.disabled': true, 'media.navigator.streams.fake': true,
    // Same-machine testing: macOS Local Network privacy may block Firefox from LAN
    // addresses, so let ICE use loopback (server runs with -loopback).
    'media.peerconnection.ice.loopback': true,
  },
});
const out = { firefox: null, runs: [] };

async function chromePage() {
  const ctx = await chrome.newContext({ viewport: { width: 1280, height: 720 }, permissions: ['microphone', 'camera'] });
  const page = await ctx.newPage();
  return { ctx, page };
}

async function waitText(page, selector, re, timeout) {
  const src = re.source;
  await page.waitForFunction((sel, s) => new RegExp(s).test(document.querySelector(sel)?.textContent || ''), { timeout }, selector, src);
}

// Firefox codec support (fresh profile: OpenH264 may still be downloading).
{
  const p = await firefox.newPage();
  await p.goto(BASE);
  out.firefox = await p.evaluate(() => ({
    ua: navigator.userAgent,
    recvH264: RTCRtpReceiver.getCapabilities('video').codecs.filter((c) => /h264/i.test(c.mimeType)).map((c) => c.sdpFmtpLine),
    sendH264: RTCRtpSender.getCapabilities('video').codecs.filter((c) => /h264/i.test(c.mimeType)).map((c) => c.sdpFmtpLine),
  }));
  await p.close();
}

// Chrome shares, Firefox watches.
for (const profile of PROFILES) {
  const { ctx, page: sharer } = await chromePage();
  const viewer = await firefox.newPage();
  try {
    await sharer.goto(`${BASE}/?autojoin`);
    await sharer.evaluate(() => navigator.mediaDevices.getUserMedia({ audio: true }).then((s) => { window.__keep = s; }));
    await sharer.selectOption('#profile', profile);
    await sharer.click('#shareCanvas');
    await sharer.waitForFunction(() => /server got video rid=f/.test(document.querySelector('#pubstats').textContent), null, { timeout: 20000 });
    await viewer.goto(`${BASE}/?autojoin&switches=${SWITCHES}`);
    await viewer.waitForFunction(() => /decoded [1-9]/.test(document.querySelector('.tile .stats')?.textContent || ''), { timeout: 20000 });
    await new Promise((r) => setTimeout(r, 3000));
    const steady = await viewer.evaluate(() => document.querySelector('.tile .stats').textContent);
    await viewer.click('.tile .auto');
    await viewer.waitForFunction(() => /auto-switch: \{/.test(document.querySelector('.tile .auto-result').textContent), { timeout: SWITCHES * 3500 + 30000 });
    const res = await viewer.evaluate(() => results);
    const pub = await sharer.evaluate(() => results.publish);
    const tile = Object.values(res.tiles)[0];
    out.runs.push({ direction: 'chrome→firefox', profile,
      sent: pub.layers.map((l) => `${l.rid} ${l.width}x${l.height} ${l.codec?.split('profile-level-id=')[1]}`),
      steady, autoSwitch: tile.autoSwitch?.summary });
  } catch (e) {
    const log = await viewer.evaluate(() => document.querySelector('#log')?.textContent.slice(0, 1500)).catch(() => '');
    const stats = await viewer.evaluate(() => document.querySelector('.tile .stats')?.textContent).catch(() => '');
    out.runs.push({ direction: 'chrome→firefox', profile, error: String(e).slice(0, 200), viewerStats: stats, viewerLog: log });
  } finally {
    await ctx.close();
    await viewer.close();
  }
}

// Firefox shares, Chrome watches.
{
  const sharer = await firefox.newPage();
  const { ctx, page: viewer } = await chromePage();
  try {
    await sharer.goto(`${BASE}/?autojoin`);
    await sharer.click('#shareCanvas');
    await sharer.waitForFunction(() => /server got video/.test(document.querySelector('#pubstats').textContent), { timeout: 20000 });
    const pubText = await sharer.evaluate(() => document.querySelector('#pubstats').textContent);
    await viewer.goto(`${BASE}/?autojoin&switches=${SWITCHES}`);
    await viewer.evaluate(() => navigator.mediaDevices.getUserMedia({ audio: true }).then((s) => { window.__keep = s; }));
    await viewer.waitForFunction(() => /decoded [1-9]/.test(document.querySelector('.tile .stats')?.textContent || ''), null, { timeout: 20000 });
    await viewer.waitForTimeout(3000);
    const steady = await viewer.evaluate(() => document.querySelector('.tile .stats').textContent);
    await viewer.click('.tile .auto');
    await viewer.waitForFunction(() => /auto-switch: \{/.test(document.querySelector('.tile .auto-result').textContent), null, { timeout: SWITCHES * 3500 + 30000 });
    const res = await viewer.evaluate(() => results);
    out.runs.push({ direction: 'firefox→chrome', pubText, steady, autoSwitch: Object.values(res.tiles)[0].autoSwitch?.summary });
  } catch (e) {
    const log = await sharer.evaluate(() => document.querySelector('#log')?.textContent.slice(0, 1500)).catch(() => '');
    out.runs.push({ direction: 'firefox→chrome', error: String(e).slice(0, 200), sharerLog: log });
  } finally {
    await sharer.close();
    await ctx.close();
  }
}

await chrome.close();
await firefox.close();
console.log(JSON.stringify(out, null, 2));
