// Jarvis Chat — mobile PWA client for the jarvis-voice service.
// Talks to /chat (text + planning) and /voice (STT→LLM→TTS) over the LAN/VPN.
// The conversation lives in localStorage; the backend stays stateless per turn.
'use strict';

const $ = (s) => document.querySelector(s);

// ---- State ----
const BASE = location.origin; // https://<ip>:8443 (or http://localhost:PORT)
const LS = {
  auth: 'jarvis.auth.v1',
  model: 'jarvis.model.v1',
  history: 'jarvis.history.v1',
  hint: 'jarvis.hint.v1',
};
let auth = null;           // "Basic " payload (base64 user:pass) or null
let models = [];
let selectedModel = null;  // model id
let history = [];          // [{role:'user'|'bot', content, voice?, actions?}]
let busy = false;
let recording = false;
let mediaRecorder = null;
let mediaStream = null;
let recChunks = [];
let recTimer = null;
let installEvt = null;

// ---- Persistence ----
function loadState() {
  try { auth = localStorage.getItem(LS.auth); } catch (e) { auth = null; }
  try { selectedModel = localStorage.getItem(LS.model) || null; } catch (e) {}
  try { history = JSON.parse(localStorage.getItem(LS.history) || '[]') || []; } catch (e) { history = []; }
}
function saveHistory() {
  try { localStorage.setItem(LS.history, JSON.stringify(history.slice(-80))); } catch (e) {}
}
function saveModel() {
  try { localStorage.setItem(LS.model, selectedModel || ''); } catch (e) {}
}

// ---- API ----
async function api(path, opts = {}) {
  const headers = Object.assign({}, opts.headers || {});
  if (auth) headers['Authorization'] = auth;
  let res;
  try {
    res = await fetch(BASE + path, Object.assign({}, opts, { headers }));
  } catch (e) {
    showOffline();
    throw new Error('Can’t reach Jarvis. You need to be on the home network or your VPN.');
  }
  hideOffline();
  if (res.status === 401) { showLogin(); throw new Error('unauthorized'); }
  if (!res.ok) {
    let detail = res.statusText;
    try { const j = await res.json(); detail = j.error || j.detail || detail; } catch (e) {}
    throw new Error(detail);
  }
  return res;
}

function setStatus(ok) {
  const el = $('#status');
  el.className = 'status ' + (ok ? 'ok' : 'bad');
  el.title = ok ? 'Connected' : 'Unreachable';
}
function showOffline() { $('#offline').hidden = false; setStatus(false); }
function hideOffline() { $('#offline').hidden = true; }
function setState(t) {
  const el = $('#state');
  el.textContent = t || '';
  el.hidden = !t;
}

// ---- Models (the picker) ----
async function loadModels() {
  try {
    const res = await api('/models');
    const data = await res.json();
    models = data.models || [];
    setStatus(true);
  } catch (e) {
    if (e.message === 'unauthorized') return; // login overlay is up
    models = [];
    return;
  }
  const sel = $('#model');
  sel.innerHTML = '';
  if (!models.length) {
    const o = document.createElement('option');
    o.value = ''; o.textContent = 'default';
    sel.appendChild(o);
    selectedModel = null;
    saveModel();
    return;
  }
  models.forEach((m) => {
    const o = document.createElement('option');
    o.value = m.id;
    o.textContent = m.name + (m.default ? ' (default)' : '');
    sel.appendChild(o);
  });
  const found = models.find((m) => m.id === selectedModel);
  sel.value = found ? found.id : (models.find((m) => m.default) || models[0]).id;
  selectedModel = sel.value;
  saveModel();
}

// ---- Rendering ----
function render() {
  const thread = $('#thread');
  thread.innerHTML = '';
  const lastBot = lastBotIndex();
  history.forEach((m, i) => thread.appendChild(messageEl(m, i, i === lastBot)));
  $('#empty').style.display = history.length ? 'none' : 'block';
  scrollBottom();
}
function lastBotIndex() {
  for (let i = history.length - 1; i >= 0; i--) if (history[i].role === 'bot') return i;
  return -1;
}
function messageEl(m, i, isLast) {
  const wrap = document.createElement('div');
  wrap.className = 'msg ' + (m.role === 'user' ? 'user' : 'bot');
  const bubble = document.createElement('div');
  bubble.className = 'bubble';
  if (m.voice) {
    const tag = document.createElement('span');
    tag.className = 'vtag';
    tag.textContent = '🎙 voice';
    bubble.appendChild(tag);
  }
  const txt = document.createElement('div');
  txt.className = 'text';
  txt.textContent = m.content || ''; // textContent — no HTML injection
  bubble.appendChild(txt);
  wrap.appendChild(bubble);

  if (m.actions && m.actions.length) {
    m.actions.forEach((a) => {
      const chip = document.createElement('div');
      chip.className = 'action ' + (a.scheduled ? 'queued' : 'search');
      chip.textContent = (a.scheduled ? '⚙ Queued: ' : '🔎 ') + short(a.result);
      chip.title = a.result || '';
      wrap.appendChild(chip);
    });
  }
  if (m.role === 'bot' && isLast && !m.error) {
    const q = document.createElement('button');
    q.className = 'queuebtn';
    q.textContent = 'Queue that as a job';
    q.onclick = sendPlanningNudge;
    wrap.appendChild(q);
  }
  return wrap;
}
function short(s) {
  s = (s || '').replace(/\s+/g, ' ').trim();
  return s.length > 90 ? s.slice(0, 87) + '…' : s;
}
function scrollBottom() {
  const t = $('#thread');
  t.scrollTop = t.scrollHeight;
}

// ---- Text chat ----
function toWire(h) {
  return h
    .filter((m) => m.role === 'user' || m.role === 'bot')
    .map((m) => ({ role: m.role === 'bot' ? 'assistant' : 'user', content: m.content }));
}
async function sendMessage(text) {
  text = (text || '').trim();
  if (!text || busy) return;
  busy = true;
  setBusy(true);
  history.push({ role: 'user', content: text });
  saveHistory();
  render();
  setState('Thinking…');
  try {
    const res = await api('/chat', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ messages: toWire(history), model: selectedModel || undefined }),
    });
    const data = await res.json();
    history.push({ role: 'bot', content: data.reply || '(no reply)', actions: data.actions || [] });
  } catch (e) {
    if (e.message === 'unauthorized') { /* login overlay handles it */ }
    else history.push({ role: 'bot', content: '⚠ ' + e.message, error: true });
  } finally {
    busy = false;
    setBusy(false);
    saveHistory();
    render();
    setState('');
  }
}
function sendPlanningNudge() {
  sendMessage('Please turn that into a job: lay out the plan, then queue it with the schedule_job tool.');
}

// ---- Voice (record → /voice → play) ----
// Auto-stop tuning — adjust here, no digging needed:
//   VAD_SILENCE_MS   — stop and send after this long without speech
//   VAD_MIN_RECORD_MS — never auto-stop before this much recording
//   VAD_THRESHOLD    — RMS (0..1) below this counts as silence; raise it for
//                      noisy rooms, lower it for quiet speakers
const VAD_SILENCE_MS = 5000;
const VAD_MIN_RECORD_MS = 2000;
const VAD_THRESHOLD = 0.01;
const VAD_TICK_MS = 100;
let vadTimer = null;
let vadCtx = null;
let vadAnalyser = null;
let vadBuf = null;
let vadLastSpeechAt = 0;
let vadStartedAt = 0;

// Voice-activity detection: watch the live mic level and auto-stop once
// silence has lasted VAD_SILENCE_MS (any speech resets the countdown, so
// pauses shorter than that never cut the utterance). Falls back to plain
// tap-to-stop if AudioContext is unavailable.
function startVad(stream) {
  try {
    const AC = window.AudioContext || window.webkitAudioContext;
    vadCtx = new AC();
    if (vadCtx.state === 'suspended') vadCtx.resume().catch(() => {});
    const src = vadCtx.createMediaStreamSource(stream);
    vadAnalyser = vadCtx.createAnalyser();
    vadAnalyser.fftSize = 2048;
    src.connect(vadAnalyser);
    vadBuf = vadAnalyser.getFloatTimeDomainData
      ? new Float32Array(vadAnalyser.fftSize)
      : new Uint8Array(vadAnalyser.fftSize); // older Safari: byte path
    const t0 = performance.now();
    vadStartedAt = t0;
    vadLastSpeechAt = t0;
    vadTimer = setInterval(vadTick, VAD_TICK_MS);
  } catch (e) {
    stopVad();
  }
}
function rmsLevel() {
  let sum = 0;
  if (vadBuf instanceof Float32Array) {
    vadAnalyser.getFloatTimeDomainData(vadBuf);
    for (let i = 0; i < vadBuf.length; i++) sum += vadBuf[i] * vadBuf[i];
  } else {
    vadAnalyser.getByteTimeDomainData(vadBuf);
    for (let i = 0; i < vadBuf.length; i++) {
      const v = (vadBuf[i] - 128) / 128;
      sum += v * v;
    }
  }
  return Math.sqrt(sum / vadBuf.length);
}
function vadTick() {
  if (!recording) return;
  const now = performance.now();
  if (rmsLevel() >= VAD_THRESHOLD) { vadLastSpeechAt = now; return; }
  if (now - vadLastSpeechAt >= VAD_SILENCE_MS &&
      now - vadStartedAt >= VAD_MIN_RECORD_MS) stopVoice();
}
function stopVad() {
  if (vadTimer) { clearInterval(vadTimer); vadTimer = null; }
  if (vadCtx) { try { vadCtx.close(); } catch (e) {} vadCtx = null; }
  vadAnalyser = null;
  vadBuf = null;
}

function pickMime() {
  const c = ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4', 'audio/ogg;codecs=opus'];
  for (const t of c) { try { if (window.MediaRecorder && MediaRecorder.isTypeSupported(t)) return t; } catch (e) {} }
  return '';
}
async function toggleVoice() {
  if (recording) { stopVoice(); return; }
  if (busy) return;
  try {
    mediaStream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true },
    });
  } catch (e) {
    history.push({ role: 'bot', content: '⚠ Microphone unavailable: ' + e.message +
      '. The app needs HTTPS and mic permission.', error: true });
    render();
    return;
  }
  const mime = pickMime();
  try {
    mediaRecorder = new MediaRecorder(mediaStream, mime ? { mimeType: mime } : undefined);
  } catch (e) {
    stopStream();
    history.push({ role: 'bot', content: '⚠ Audio recording is not supported in this browser.', error: true });
    render();
    return;
  }
  recChunks = [];
  mediaRecorder.ondataavailable = (ev) => { if (ev.data && ev.data.size) recChunks.push(ev.data); };
  mediaRecorder.onstop = onVoiceStopped;
  mediaRecorder.start();
  recording = true;
  startVad(mediaStream); // silence auto-stop; no-op fallback if unavailable
  setMic(true);
  recTimer = setTimeout(() => { if (recording) stopVoice(); }, 30000); // safety stop
}
function stopVoice() {
  if (recTimer) { clearTimeout(recTimer); recTimer = null; }
  stopVad();
  if (mediaRecorder && mediaRecorder.state !== 'inactive') { try { mediaRecorder.stop(); } catch (e) {} }
  stopStream();
  recording = false;
  setMic(false);
}
function stopStream() {
  if (mediaStream) { mediaStream.getTracks().forEach((t) => t.stop()); mediaStream = null; }
}
async function onVoiceStopped() {
  const type = (mediaRecorder && mediaRecorder.mimeType) || 'audio/webm';
  const blob = new Blob(recChunks, { type });
  recChunks = [];
  if (!blob.size) { setState(''); return; }
  busy = true;
  setBusy(true);
  setState('Listening… processing');
  try {
    const fd = new FormData();
    fd.append('file', blob, 'voice.webm');
    if (selectedModel) fd.append('model', selectedModel);
    const res = await api('/voice', { method: 'POST', body: fd });
    const transcript = res.headers.get('X-Transcript') || '';
    const reply = res.headers.get('X-Reply') || '';
    const audio = await res.blob();
    history.push({ role: 'user', content: transcript || '(voice)', voice: true });
    history.push({ role: 'bot', content: reply || '(no reply)', voice: true });
    saveHistory();
    render();
    playAudio(audio);
  } catch (e) {
    if (e.message !== 'unauthorized') history.push({ role: 'bot', content: '⚠ ' + e.message, error: true });
    render();
  } finally {
    busy = false;
    setBusy(false);
    if (!recording) setState('');
  }
}
function playAudio(blob) {
  const url = URL.createObjectURL(blob);
  const a = new Audio(url);
  setState('Speaking…');
  const done = () => { setState(''); URL.revokeObjectURL(url); };
  a.onended = done;
  a.onerror = done;
  a.play().catch(() => done());
}

// ---- UI wiring ----
function setBusy(b) {
  $('#send').disabled = b;
  $('#plan').disabled = b;
  $('#input').disabled = b;
}
function setMic(on) {
  const m = $('#mic');
  m.classList.toggle('on', on);
  m.textContent = on ? '⏹' : '🎙';
  if (on) setState(vadTimer ? 'Listening… auto-stops after silence' : 'Listening… tap to stop');
  else if (!busy) setState('');
}

// ---- Auth ----
function showLogin() { $('#login').hidden = false; }
function doLogin() {
  const u = $('#lu').value.trim();
  const p = $('#lp').value;
  if (!u) return;
  auth = 'Basic ' + btoa(u + ':' + p);
  try { localStorage.setItem(LS.auth, auth); } catch (e) {}
  $('#login').hidden = true;
  $('#logout').style.display = 'inline-grid';
  loadModels();
}
function doLogout() {
  auth = null;
  try { localStorage.removeItem(LS.auth); } catch (e) {}
  $('#lp').value = '';
  showLogin();
}

// ---- Install ----
function maybeInstallHint() {
  try { if (localStorage.getItem(LS.hint)) return; } catch (e) {}
  const standalone = window.matchMedia && window.matchMedia('(display-mode: standalone)').matches;
  const ios = /iphone|ipad|ipod/i.test(navigator.userAgent) ||
    (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  if (!standalone && ios) $('#instHint').hidden = false;
}

// ---- Service worker (offline app shell) ----
function registerSW() {
  if ('serviceWorker' in navigator &&
      (location.protocol === 'https:' || location.hostname === 'localhost')) {
    navigator.serviceWorker.register('/app/sw.js').catch(() => {});
  }
}

// ---- Boot ----
function bindEvents() {
  const input = $('#input');
  const go = () => { const v = input.value; input.value = ''; sendMessage(v); };
  $('#send').onclick = go;
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); go(); } });
  $('#mic').onclick = toggleVoice;
  $('#plan').onclick = sendPlanningNudge;
  $('#newchat').onclick = () => {
    if (busy) return;
    if (confirm('Start a new conversation?')) { history = []; saveHistory(); render(); }
  };
  $('#model').onchange = () => { selectedModel = $('#model').value; saveModel(); };
  $('#logout').onclick = doLogout;
  $('#loginForm').onsubmit = (e) => { e.preventDefault(); doLogin(); };
  $('#dismissHint').onclick = () => {
    $('#instHint').hidden = true;
    try { localStorage.setItem(LS.hint, '1'); } catch (e) {}
  };
  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault();
    installEvt = e;
    const b = $('#installBtn');
    b.hidden = false;
    b.onclick = async () => {
      if (installEvt) { installEvt.prompt(); await new Promise((r) => setTimeout(r, 500)); installEvt = null; }
      b.hidden = true;
    };
  });
  window.addEventListener('appinstalled', () => { $('#installBtn').hidden = true; });
}

async function boot() {
  loadState();
  bindEvents();
  if (auth) $('#logout').style.display = 'inline-grid';
  render();
  await loadModels();
  maybeInstallHint();
}

document.addEventListener('DOMContentLoaded', () => {
  registerSW();
  boot();
});
