// Thin client for the brain's chat API (scripts/voice_service.py in this repo).
// The endpoints are shared with the PWA (scripts/jarvis_app) and are NOT
// changed by the app:
//   POST /chat    {"messages":[{role,content}...], "model"?} -> {reply, scheduled, actions}
//   POST /voice   multipart audio in -> audio out + X-Transcript / X-Reply headers
//   GET  /models  -> {models:[{id,name,default}], default}
//   GET  /health  -> liveness probe for the offline banner
//
// Auth mirrors the PWA: optional HTTP Basic. The service's auth is off by
// default (HA integration sends no credentials), so blank fields work.

import { Platform } from 'react-native';

const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

function b64Encode(str) {
  // UTF-8-safe base64. Hand-rolled because btoa is not guaranteed on every
  // RN/Hermes runtime; one implementation for web + native.
  const bytes = [];
  for (let i = 0; i < str.length; i++) {
    const c = str.codePointAt(i);
    if (c > 0xffff) i++; // second half of a surrogate pair
    if (c < 0x80) bytes.push(c);
    else if (c < 0x800) bytes.push(0xc0 | (c >> 6), 0x80 | (c & 63));
    else if (c < 0x10000) bytes.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
    else bytes.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
  }
  let out = '';
  for (let i = 0; i < bytes.length; i += 3) {
    const b0 = bytes[i];
    const b1 = i + 1 < bytes.length ? bytes[i + 1] : 0;
    const b2 = i + 2 < bytes.length ? bytes[i + 2] : 0;
    const pad = bytes.length - i;
    out += B64[b0 >> 2];
    out += B64[((b0 & 3) << 4) | (b1 >> 4)];
    out += pad > 1 ? B64[((b1 & 15) << 2) | (b2 >> 6)] : '=';
    out += pad > 2 ? B64[b2 & 63] : '=';
  }
  return out;
}

export function headers(cfg) {
  const h = {};
  if (cfg.authUser && cfg.authPass) {
    h.Authorization = 'Basic ' + b64Encode(cfg.authUser + ':' + cfg.authPass);
  }
  return h;
}

export function url(cfg, path) {
  return cfg.serverUrl.replace(/\/+$/, '') + path;
}

function withTimeout(ms) {
  if (typeof AbortController === 'undefined') return { signal: undefined, done: () => {} };
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), ms);
  return { signal: ctl.signal, done: () => clearTimeout(t) };
}

async function request(cfg, path, opts = {}, timeoutMs = 60000) {
  const to = withTimeout(timeoutMs);
  try {
    // Content-Type only for JSON bodies — FormData (multipart uploads)
    // supplies its own Content-Type with the boundary.
    const h = { ...headers(cfg), ...(opts.headers || {}) };
    if (opts.body && !h['Content-Type']) h['Content-Type'] = 'application/json';
    const res = await fetch(url(cfg, path), {
      method: opts.method || 'GET',
      headers: h,
      body: opts.body,
      signal: to.signal,
    });
    let body = null;
    try { body = await res.json(); } catch (e) { /* not JSON */ }
    if (!res.ok) {
      const msg = (body && body.error) || res.statusText || 'HTTP ' + res.status;
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    return body;
  } finally {
    to.done();
  }
}

// GET /models — the model picker's data source. Grows automatically when the
// engine serves more models; no app update needed.
export function fetchModels(cfg) {
  return request(cfg, '/models', {}, 10000);
}

// POST /chat — stateless: the client owns the conversation and sends the full
// history. Returns {reply, scheduled, actions}; actions lists every tool run
// (schedule_job / search_jobs) so the UI can show "⚙ Queued: …" chips.
export function sendChat(cfg, messages, model) {
  const body = { messages: messages.map((m) => ({ role: m.role, content: m.content })) };
  if (model) body.model = model;
  return request(cfg, '/chat', { method: 'POST', body: JSON.stringify(body) });
}

// GET /health — cheap liveness probe behind the offline/LAN banner.
export async function ping(cfg, timeoutMs = 5000) {
  try {
    await request(cfg, '/health', {}, timeoutMs);
    return true;
  } catch (e) {
    return false;
  }
}

// The default brain on this box: LAN-only, self-signed TLS (same cert the
// PWA and the browser voice-chat page use). Changeable in ⚙ Settings.
//
// Self-signed cert handling, per platform:
// - iOS + web use the HTTPS endpoint (8443). The phone already trusts the LAN
//   CA profile (that is how the PWA works); iOS trusts user-installed CAs
//   for app connections.
// - Android does not trust user-installed CAs in apps, so it uses the same
//   service's plain-HTTP port (8100, published in docker-compose.yml) — no
//   certificate involved at all. Same backend, same endpoints.
export const DEFAULT_SERVER =
  Platform.OS === 'android'
    ? 'http://111.1.2.27:8100'
    : 'https://111.1.2.27:8443';
