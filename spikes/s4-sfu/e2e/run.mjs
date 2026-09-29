// Drives real Google Chrome (headless) against the spike: one page shares an
// animated canvas test pattern + tone with simulcast H.264, a second page watches it and
// runs the auto-switch test. Repeats per H.264 profile. Usage: npm run e2e
import { chromium } from 'playwright-core';

const BASE = process.env.BASE || 'http://localhost:8080';
const PROFILES = (process.env.PROFILES || 'auto,42e01f,4d001f,64001f').split(',');
const SWITCHES = Number(process.env.SWITCHES || 10);

const browser = await chromium.launch({
  channel: 'chrome',
  headless: !process.env.HEADFUL,
  args: ['--autoplay-policy=no-user-gesture-required', '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'],
});

const summary = [];
for (const profile of PROFILES) {
  const ctxOpts = { viewport: { width: 1280, height: 720 }, permissions: ['microphone', 'camera'] };
  const sharerCtx = await browser.newContext(ctxOpts);
  const viewerCtx = await browser.newContext(ctxOpts);
  const sharer = await sharerCtx.newPage();
  const viewer = await viewerCtx.newPage();
  const logs = [];
  for (const [name, page] of [['sharer', sharer], ['viewer', viewer]]) {
    page.on('console', (m) => logs.push(`${name}: ${m.text()}`));
  }
  try {
    await sharer.goto(`${BASE}/?autojoin`);
    // Chrome only exposes encoder/decoder implementation to pages with an active capture.
    for (const page of [sharer, viewer]) {
      page.on('load', () => page.evaluate(() => navigator.mediaDevices.getUserMedia({ audio: true }).then((s) => { window.__keep = s; })).catch(() => {}));
    }
    await sharer.evaluate(() => navigator.mediaDevices.getUserMedia({ audio: true }).then((s) => { window.__keep = s; }));
    await sharer.selectOption('#profile', profile);
    await sharer.click('#shareCanvas');
    await sharer.waitForFunction(() => /server got video rid=f/.test(document.querySelector('#pubstats').textContent), null, { timeout: 20000 });

    await viewer.goto(`${BASE}/?autojoin&switches=${SWITCHES}`);
    await viewer.waitForFunction(() => /decoded [1-9]/.test(document.querySelector('.tile .stats')?.textContent || ''), null, { timeout: 20000 });
    await viewer.waitForTimeout(3000); // let the high layer settle
    const steady = await viewer.evaluate(() => document.querySelector('.tile .stats').textContent);
    await viewer.click('.tile .auto');
    await viewer.waitForFunction(() => /auto-switch: \{/.test(document.querySelector('.tile .auto-result').textContent), null, { timeout: SWITCHES * 3500 + 30000 });
    await viewer.waitForTimeout(1500);
    const res = await viewer.evaluate(() => results);
    const pub = await sharer.evaluate(() => results.publish);
    const tile = Object.values(res.tiles)[0];
    summary.push({
      profile,
      publisher: pub.layers.map((l) => `${l.rid}: ${l.width}x${l.height}@${l.fps} ${l.encoder}${l.hw ? ' (hw)' : ''} ${l.codec}`),
      viewer: {
        codec: tile.client?.codec, decoder: tile.client?.decoder, hwDecoder: tile.client?.powerEfficientDecoder,
        keyFramesDecoded: tile.client?.keyFramesDecoded, pli: tile.client?.pliCount,
        framesDecoded: tile.client?.framesDecoded, freezes: tile.client?.freezeCount,
        serverMatch: `${tile.server?.match} (${tile.server?.profile})`,
      },
      steady,
      autoSwitch: tile.autoSwitch?.summary,
    });
  } catch (e) {
    summary.push({ profile, error: String(e), lastLogs: logs.slice(-15) });
  } finally {
    await sharerCtx.close();
    await viewerCtx.close();
  }
}
await browser.close();
console.log(JSON.stringify(summary, null, 2));
