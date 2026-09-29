'use strict';
// Spike S4 test client. Deliberately framework-free; the real client is React (plan M1).

const $ = (s, root = document) => root.querySelector(s);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let ws = null;
let myId = null;
let pub = null;
let sub = null;
let localStream = null;
let focused = null;
let serverStats = { stats: [], layers: [] };
const tiles = new Map(); // shareId -> tile
const pendingICE = { pub: [], sub: [] };
const results = { userAgent: navigator.userAgent, publish: null, tiles: {} };

function log(...args) {
  const line = args.map((a) => (typeof a === 'string' ? a : JSON.stringify(a))).join(' ');
  console.log(line);
  const el = $('#log');
  el.textContent = `${new Date().toISOString().slice(11, 23)} ${line}\n${el.textContent}`.slice(0, 30000);
}

function send(m) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(m));
}

// ---------- signaling ----------

function connect() {
  if (ws) return;
  ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws`);
  ws.onopen = () => send({ type: 'join', name: $('#name').value || browserName() });
  ws.onclose = () => { log('signaling closed'); ws = null; };
  ws.onmessage = async (ev) => {
    const m = JSON.parse(ev.data);
    try { await onMessage(m); } catch (e) { log('error handling', m.type, String(e)); }
  };
}

async function onMessage(m) {
  switch (m.type) {
    case 'welcome':
      myId = m.id;
      $('#me').textContent = `connected as ${myId}`;
      break;
    case 'state':
      renderState(m.state);
      break;
    case 'pub.answer':
      await pub.setRemoteDescription({ type: 'answer', sdp: m.sdp });
      await flushICE('pub');
      logNegotiated('pub', pub);
      break;
    case 'sub.offer':
      await onSubOffer(m.sdp);
      break;
    case 'ice': {
      const pc = m.pc === 'pub' ? pub : sub;
      if (pc && pc.remoteDescription) await pc.addIceCandidate(m.candidate);
      else pendingICE[m.pc].push(m.candidate);
      break;
    }
    case 'stats':
      serverStats = m;
      break;
    case 'error':
      log('server error:', m.error);
      break;
  }
}

async function flushICE(name) {
  const pc = name === 'pub' ? pub : sub;
  const list = pendingICE[name].splice(0);
  for (const c of list) {
    try { await pc.addIceCandidate(c); } catch (e) { log('addIceCandidate', name, String(e)); }
  }
}

function newPC(name) {
  const pc = new RTCPeerConnection();
  pc.onicecandidate = (e) => e.candidate && send({ type: 'ice', pc: name, candidate: e.candidate.toJSON() });
  pc.onconnectionstatechange = async () => {
    log(`${name} pc: ${pc.connectionState}`);
    if (pc.connectionState === 'failed' || pc.connectionState === 'connected') await logCandidates(name, pc);
    if (pc.connectionState === 'failed') {
      $('#banner').hidden = false;
      $('#banner').textContent = `The ${name === 'pub' ? 'sharing' : 'watching'} connection to the server failed (no media path). ` +
        'On Windows: allow s4-sfu.exe in Windows Firewall (Private networks) and reload. Then use "Copy results JSON" — it includes the log.';
    }
  };
  return pc;
}

// ICE diagnostics: which candidates each side offered and which pair (if any) worked.
async function logCandidates(name, pc) {
  const report = await pc.getStats();
  const cands = {};
  report.forEach((s) => { if (s.type === 'local-candidate' || s.type === 'remote-candidate') cands[s.id] = s; });
  const fmt = (c) => c ? `${c.candidateType} ${c.protocol} ${c.address || c.ip}:${c.port}` : '?';
  const lines = [];
  report.forEach((s) => {
    if (s.type === 'candidate-pair') lines.push(`${s.state}${s.nominated ? '*' : ''} ${fmt(cands[s.localCandidateId])} -> ${fmt(cands[s.remoteCandidateId])}`);
  });
  const remote = Object.values(cands).filter((c) => c.type === 'remote-candidate').map(fmt);
  log(`${name} ICE pairs: ${lines.join(' | ') || 'none'} ; remote candidates: ${remote.join(', ')}`);
}

async function onSubOffer(sdp) {
  if (!sub) {
    sub = newPC('sub');
    sub.ontrack = onRemoteTrack;
  }
  await sub.setRemoteDescription({ type: 'offer', sdp });
  await flushICE('sub');
  const answer = await sub.createAnswer();
  answer.sdp = forceOpusStereo(answer.sdp); // receivers must ask for stereo themselves
  await sub.setLocalDescription(answer);
  send({ type: 'sub.answer', sdp: sub.localDescription.sdp });
}

function forceOpusStereo(sdp) {
  const m = /a=rtpmap:(\d+) opus\/48000\/2/i.exec(sdp);
  if (!m) return sdp;
  return sdp.replace(new RegExp(`a=fmtp:${m[1]} ([^\\r\\n]*)`), (line, params) =>
    /stereo=1/.test(params) ? line : `a=fmtp:${m[1]} ${params};stereo=1;sprop-stereo=1`);
}

function logNegotiated(name, pc) {
  const lines = (pc.remoteDescription?.sdp || '').split(/\r?\n/).filter((l) => /^a=(rtpmap|fmtp|simulcast|rid)/.test(l));
  log(`${name} negotiated:`, lines.filter((l) => /H264|opus|simulcast|rid/i.test(l)).slice(0, 12).join(' | '));
}

// ---------- viewing ----------

function renderState(state) {
  const live = new Set(state.shares.map((s) => s.id));
  for (const [id, t] of tiles) {
    if (!live.has(id)) {
      t.el.remove();
      tiles.delete(id);
      if (focused === t) focused = null;
    }
  }
  for (const info of state.shares) {
    let t = tiles.get(info.id);
    if (!t) {
      t = makeTile(info);
      tiles.set(info.id, t);
      $('#grid').append(t.el);
      if (!focused) focusTile(t); else setWanted(t, 'low', false);
    }
    t.info = info;
    $('.title', t.el).textContent = `${info.name}${info.peerId === myId ? ' (you)' : ''}`;
    $('.meta', t.el).textContent = `layers [${info.layers.join(',') || '-'}] · ${info.profile || info.codec} · audio ${info.audio ? 'yes' : 'no'}`;
  }
}

function makeTile(info) {
  const el = document.createElement('div');
  el.className = 'tile';
  el.innerHTML = `<div><b class="title"></b> <span class="meta muted"></span></div>
    <video autoplay playsinline muted></video>
    <div>
      <button data-q="high">High</button><button data-q="low">Low</button><button data-q="off">Off</button>
      <label><input type="checkbox" class="aud"> audio</label>
      <button class="focus">Focus</button>
      <button class="auto">Auto-switch test</button>
    </div>
    <pre class="stats"></pre><pre class="auto-result"></pre>`;
  const t = { info, el, video: $('video', el), stream: new MediaStream(), videoMid: null, videoTrack: null, quality: 'low', audio: false, prev: null };
  t.video.srcObject = t.stream;
  el.querySelectorAll('[data-q]').forEach((b) => b.addEventListener('click', () => setWanted(t, b.dataset.q, t.audio)));
  $('.aud', el).addEventListener('change', (e) => setWanted(t, t.quality, e.target.checked));
  $('.focus', el).addEventListener('click', () => focusTile(t));
  $('.auto', el).addEventListener('click', () => autoSwitchTest(t));
  return t;
}

// Audio follows focus: the focused share is high + audible, others are low + muted.
function focusTile(t) {
  focused = t;
  for (const other of tiles.values()) {
    setWanted(other, other === t ? 'high' : (other.quality === 'off' ? 'off' : 'low'), other === t);
    other.el.classList.toggle('focused', other === t);
  }
}

function setWanted(t, quality, audio) {
  t.quality = quality;
  t.audio = audio;
  t.video.muted = !audio;
  if (audio) t.video.play().catch(() => {});
  $('.aud', t.el).checked = audio;
  t.el.querySelectorAll('[data-q]').forEach((b) => b.classList.toggle('on', b.dataset.q === quality));
  send({ type: 'subscribe', share: t.info.id, video: quality, audio });
}

function onRemoteTrack(e) {
  const shareId = e.streams[0]?.id;
  const t = tiles.get(shareId);
  if (!t) { log('track for unknown share', shareId); return; }
  for (const old of t.stream.getTracks().filter((x) => x.kind === e.track.kind)) t.stream.removeTrack(old);
  t.stream.addTrack(e.track);
  if (e.track.kind === 'video') { t.videoMid = e.transceiver.mid; t.videoTrack = e.track; }
  log(`receiving ${e.track.kind} for ${shareId} (mid ${e.transceiver.mid})`);
}

async function videoStats(t) {
  if (!sub || !t.videoTrack) return null;
  const report = await sub.getStats();
  let inbound = null;
  report.forEach((s) => {
    if (s.type === 'inbound-rtp' && s.kind === 'video' && (s.mid === t.videoMid || s.trackIdentifier === t.videoTrack.id)) inbound = s;
  });
  if (!inbound) return null;
  const codec = inbound.codecId ? report.get(inbound.codecId) : null;
  return {
    at: performance.now(),
    width: inbound.frameWidth, height: inbound.frameHeight, fps: inbound.framesPerSecond,
    framesReceived: inbound.framesReceived, framesDecoded: inbound.framesDecoded, framesDropped: inbound.framesDropped,
    keyFramesDecoded: inbound.keyFramesDecoded, freezeCount: inbound.freezeCount, totalFreezesDuration: inbound.totalFreezesDuration,
    pliCount: inbound.pliCount, nackCount: inbound.nackCount, packetsLost: inbound.packetsLost,
    jitterBufferMs: inbound.jitterBufferEmittedCount ? Math.round(1000 * inbound.jitterBufferDelay / inbound.jitterBufferEmittedCount) : undefined,
    bytesReceived: inbound.bytesReceived,
    decoder: inbound.decoderImplementation, powerEfficientDecoder: inbound.powerEfficientDecoder,
    codec: codec ? `${codec.mimeType} ${codec.sdpFmtpLine || ''}` : undefined,
  };
}

function fmtStats(s, prev, server) {
  if (!s) return 'no inbound video yet';
  const kbps = prev ? Math.round(8 * (s.bytesReceived - prev.bytesReceived) / (s.at - prev.at)) : 0;
  const undecoded = s.framesReceived != null ? s.framesReceived - s.framesDecoded - (s.framesDropped || 0) : 'n/a';
  const lines = [
    `${s.width || '?'}x${s.height || '?'} @ ${s.fps ?? '?'} fps · ${kbps} kbps · jitterbuf ${s.jitterBufferMs ?? '?'} ms`,
    `decoded ${s.framesDecoded} · keyframes ${s.keyFramesDecoded ?? '?'} · dropped ${s.framesDropped ?? '?'} · undecoded ${undecoded}`,
    `freezes ${s.freezeCount ?? '?'} (${(s.totalFreezesDuration ?? 0).toFixed?.(1)} s) · PLI ${s.pliCount ?? '?'} · NACK ${s.nackCount ?? '?'} · lost ${s.packetsLost}`,
    `codec ${s.codec || '?'} · decoder ${s.decoder || '?'}${s.powerEfficientDecoder ? ' (hw)' : ''}`,
  ];
  if (server) lines.push(`server: want ${server.quality} → layer "${server.current ?? ''}" (target "${server.target}") · switches ${server.switches} · match ${server.match} (${server.profile}) · sent ${server.sent} · dropped ${server.dropped}`);
  return lines.join('\n');
}

async function statsLoop() {
  for (;;) {
    await sleep(1000);
    for (const t of tiles.values()) {
      const s = await videoStats(t).catch(() => null);
      const server = serverStats.stats?.find((x) => x.share === t.info.id && x.kind === 'video');
      $('.stats', t.el).textContent = fmtStats(s, t.prev, server);
      if (s) t.prev = s;
      results.tiles[t.info.id] = { ...results.tiles[t.info.id], share: t.info, client: s, server };
    }
    if (pub) await publishStats();
  }
}

// Toggle high/low repeatedly; measure time until the decoded resolution changes
// and whether any freezes or undecodable frames appear around switches.
async function autoSwitchTest(t, switches = Number(new URLSearchParams(location.search).get('switches')) || 20, periodMs = 3000) {
  const out = $('.auto-result', t.el);
  const runs = [];
  let q = t.quality === 'high' ? 'low' : 'high';
  for (let i = 0; i < switches; i++) {
    const before = await videoStats(t);
    if (!before) { out.textContent = 'no video yet'; return; }
    const t0 = performance.now();
    setWanted(t, q, t.audio);
    let switchedMs = null;
    while (performance.now() - t0 < periodMs) {
      await sleep(50);
      const s = await videoStats(t);
      if (switchedMs == null && s && s.width !== before.width && s.framesDecoded > before.framesDecoded) switchedMs = Math.round(performance.now() - t0);
    }
    const after = await videoStats(t);
    runs.push({
      to: q, switchedMs,
      freezes: (after.freezeCount ?? 0) - (before.freezeCount ?? 0),
      undecoded: after.framesReceived != null
        ? (after.framesReceived - after.framesDecoded - (after.framesDropped || 0)) - (before.framesReceived - before.framesDecoded - (before.framesDropped || 0))
        : null,
    });
    out.textContent = `auto-switch ${i + 1}/${switches}…`;
    q = q === 'high' ? 'low' : 'high';
  }
  const ok = runs.filter((r) => r.switchedMs != null).map((r) => r.switchedMs).sort((a, b) => a - b);
  const summary = {
    switches, detected: ok.length,
    medianMs: ok[Math.floor(ok.length / 2)] ?? null, maxMs: ok[ok.length - 1] ?? null,
    freezes: runs.reduce((a, r) => a + r.freezes, 0),
    undecodedDelta: runs.reduce((a, r) => a + (r.undecoded || 0), 0),
  };
  results.tiles[t.info.id] = { ...(results.tiles[t.info.id] || {}), autoSwitch: { summary, runs } };
  out.textContent = `auto-switch: ${JSON.stringify(summary)}`;
  log('auto-switch result', summary);
}

// ---------- sharing ----------

// mode: 'screen' (picker), 'tab' (this tab), 'canvas' (test pattern + tone; no picker,
// so it also works in automated/headless browsers).
async function startShare(mode) {
  connect();
  const tabTest = mode === 'tab';
  const fps = Number($('#fps').value);
  const maxBitrate = Number($('#bitrate').value) * 1000;
  const simulcast = $('#simulcast').checked;
  const constraints = {
    video: { frameRate: { ideal: fps, max: fps } },
    audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false },
    systemAudio: 'include', windowAudio: 'window', surfaceSwitching: 'include',
    selfBrowserSurface: tabTest ? 'include' : 'exclude', preferCurrentTab: tabTest,
  };
  if (navigator.mediaDevices.getSupportedConstraints().restrictOwnAudio) constraints.audio.restrictOwnAudio = true;
  localStream = mode === 'canvas' ? testPatternStream(fps) : await navigator.mediaDevices.getDisplayMedia(constraints);
  const vt = localStream.getVideoTracks()[0];
  if ($('#hint').value) vt.contentHint = $('#hint').value;
  vt.onended = stopShare;

  pub = newPC('pub');
  const encodings = simulcast
    ? [{ rid: 'q', scaleResolutionDownBy: 4, maxBitrate: 300_000, maxFramerate: 15 },
       { rid: 'f', scaleResolutionDownBy: 1, maxBitrate, maxFramerate: fps }]
    : [{ maxBitrate, maxFramerate: fps }];
  let vtr;
  try {
    vtr = pub.addTransceiver(vt, { direction: 'sendonly', streams: [localStream], sendEncodings: encodings });
  } catch (e) {
    log('simulcast not accepted, single layer:', String(e));
    vtr = pub.addTransceiver(vt, { direction: 'sendonly', streams: [localStream], sendEncodings: [{ maxBitrate, maxFramerate: fps }] });
  }
  preferH264(vtr, $('#profile').value);
  const at = localStream.getAudioTracks()[0];
  if (at) {
    at.contentHint = 'music';
    pub.addTransceiver(at, { direction: 'sendonly', streams: [localStream] });
  } else {
    log('no audio captured (browser/OS may not support display audio)');
  }
  const offer = await pub.createOffer();
  await pub.setLocalDescription(offer);
  send({ type: 'pub.offer', sdp: pub.localDescription.sdp });
  $('#stop').disabled = false;
}

function preferH264(transceiver, profile) {
  if (!transceiver.setCodecPreferences || !RTCRtpSender.getCapabilities) { log('setCodecPreferences unsupported'); return; }
  const codecs = RTCRtpSender.getCapabilities('video').codecs;
  const plid = (c) => ((/profile-level-id=([0-9a-f]{6})/i.exec(c.sdpFmtpLine || '') || [])[1] || '').toLowerCase();
  const h264 = codecs.filter((c) => c.mimeType.toLowerCase() === 'video/h264' && /packetization-mode=1/.test(c.sdpFmtpLine || ''));
  if (!h264.length) { log('this browser offers no H.264 encoder'); return; }
  if (profile !== 'auto' && !h264.some((c) => plid(c).slice(0, 4) === profile.slice(0, 4))) {
    log(`this browser can't encode H.264 ${profile}; offered: ${h264.map(plid).join(',')}`);
  }
  if (profile !== 'auto') h264.sort((a, b) => (plid(b).slice(0, 4) === profile.slice(0, 4)) - (plid(a).slice(0, 4) === profile.slice(0, 4)));
  const rtx = codecs.filter((c) => c.mimeType.toLowerCase() === 'video/rtx');
  try {
    transceiver.setCodecPreferences([...h264, ...rtx]);
    log('H.264 preference order:', h264.map(plid).join(','));
  } catch (e) {
    log('setCodecPreferences failed:', String(e));
  }
}

async function publishStats() {
  const report = await pub.getStats();
  const lines = [];
  const layers = [];
  report.forEach((s) => {
    if (s.type === 'outbound-rtp' && s.kind === 'video') {
      const codec = s.codecId ? report.get(s.codecId) : null;
      layers.push({
        rid: s.rid, width: s.frameWidth, height: s.frameHeight, fps: s.framesPerSecond,
        encoder: s.encoderImplementation, hw: s.powerEfficientEncoder, limit: s.qualityLimitationReason,
        codec: codec ? `${codec.mimeType} ${codec.sdpFmtpLine || ''}` : undefined, bytesSent: s.bytesSent,
      });
    }
  });
  for (const l of layers.sort((a, b) => (a.rid || '').localeCompare(b.rid || ''))) {
    lines.push(`out rid=${l.rid ?? '-'} ${l.width}x${l.height} @ ${l.fps ?? '?'} fps · encoder ${l.encoder}${l.hw ? ' (hw)' : ''} · limit ${l.limit} · ${l.codec}`);
  }
  for (const l of serverStats.layers || []) lines.push(`server got ${l.kind} rid=${l.rid || '-'} packets ${l.packets}`);
  $('#pubstats').textContent = lines.join('\n');
  results.publish = { layers, server: serverStats.layers };
}

function stopShare() {
  if (!pub) return;
  localStream?.getTracks().forEach((t) => t.stop());
  pub.close();
  pub = null;
  localStream = null;
  pendingICE.pub = [];
  send({ type: 'unshare' });
  $('#stop').disabled = true;
  $('#pubstats').textContent = '';
}

// ---------- test pattern + tone (for "share this tab") ----------

function drawMotion() {
  const c = $('#motion');
  const g = c.getContext('2d');
  let frame = 0;
  const loop = () => {
    frame++;
    const w = c.width, h = c.height;
    g.fillStyle = `hsl(${frame % 360} 60% 20%)`;
    g.fillRect(0, 0, w, h);
    g.fillStyle = '#fff';
    g.fillRect((frame * 6) % w, h / 2 - 40, 60, 80);
    g.font = 'bold 48px monospace';
    g.fillText(`frame ${frame}`, 20, 60);
    g.font = '20px monospace';
    g.fillText(new Date().toISOString().slice(11, 23), 20, h - 20);
    requestAnimationFrame(loop);
  };
  loop();
}

function testPatternStream(fps) {
  const video = $('#motion').captureStream(fps).getVideoTracks();
  const ctx = new AudioContext();
  const osc = ctx.createOscillator();
  const dest = ctx.createMediaStreamDestination();
  osc.frequency.value = 1000;
  osc.connect(dest);
  osc.start();
  return new MediaStream([...video, ...dest.stream.getAudioTracks()]);
}

let toneCtx = null;
function toggleTone() {
  if (toneCtx) { toneCtx.close(); toneCtx = null; $('#tone').textContent = 'Play test tone'; return; }
  toneCtx = new AudioContext();
  const osc = toneCtx.createOscillator();
  const gain = toneCtx.createGain();
  osc.frequency.value = 1000;
  gain.gain.value = 0.1;
  osc.connect(gain).connect(toneCtx.destination);
  osc.start();
  $('#tone').textContent = 'Stop test tone';
}

function browserName() {
  const ua = navigator.userAgent;
  if (/Firefox\//.test(ua)) return 'firefox';
  if (/Edg\//.test(ua)) return 'edge';
  if (/Chrome\//.test(ua)) return 'chrome';
  if (/Safari\//.test(ua)) return /Mobile/.test(ua) ? 'ios-safari' : 'safari';
  return 'browser';
}

$('#name').value = browserName();
$('#join').addEventListener('click', connect);
for (const [id, mode] of [['#shareScreen', 'screen'], ['#shareTab', 'tab'], ['#shareCanvas', 'canvas']]) {
  $(id).addEventListener('click', () => startShare(mode).catch((e) => log('share failed:', String(e))));
}
$('#stop').addEventListener('click', stopShare);
$('#tone').addEventListener('click', toggleTone);
$('#copy').addEventListener('click', async () => {
  results.log = $('#log').textContent.split('\n').slice(0, 80);
  const text = JSON.stringify(results, null, 2);
  try { await navigator.clipboard.writeText(text); log('results copied to clipboard'); } catch { log(text); }
});
drawMotion();
statsLoop();
// Automation (for unattended runs): ?autojoin, ?autoshare=canvas&profile=42e01f,
// ?autotest=N (run N auto-switches on the first tile), results POSTed to /report.
const params = new URLSearchParams(location.search);
async function report(reason) {
  results.reason = reason;
  results.log = $('#log').textContent.split('\n').slice(0, 120);
  try {
    const caps = RTCRtpReceiver.getCapabilities('video').codecs.filter((c) => /h264/i.test(c.mimeType));
    results.receiveH264 = caps.map((c) => c.sdpFmtpLine);
  } catch {}
  await fetch('/report', { method: 'POST', body: JSON.stringify(results) }).catch(() => {});
}
if (params.has('autojoin') || params.has('autoshare') || params.has('autotest')) connect();
if (params.get('autoshare') === 'canvas') {
  if (params.get('profile')) $('#profile').value = params.get('profile');
  setTimeout(() => startShare('canvas').catch((e) => log('share failed:', String(e))), 1500);
  setTimeout(() => report('sharer-status'), 25000);
}
if (params.has('autotest')) {
  const n = Number(params.get('autotest')) || 6;
  let done = false;
  const deadline = setTimeout(() => { if (!done) { done = true; report('timeout: no decodable tile'); } }, 90000);
  (async () => {
    for (;;) {
      await sleep(1000);
      const t = [...tiles.values()][0];
      const s = t && await videoStats(t).catch(() => null);
      if (s && s.framesDecoded > 30) {
        await autoSwitchTest(t, n);
        if (!done) { done = true; clearTimeout(deadline); await report('autotest complete'); }
        return;
      }
    }
  })();
}
