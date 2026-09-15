// Reference game shell for the Kobra package test game.
//
// It implements the boot sequence of FS §10.1 against the real launcher, so the
// fixture exercises the whole contract rather than a happy-path subset:
//
//   1. read the bootstrap token from the URL fragment
//   2. POST /__kobra/session            -> {session_id, csrf_token, api_version}
//   3. history.replaceState()           -> erase the fragment
//   4. GET  /api/state                  -> api_version + data_writable
//   5. GET  /api/data/config, /api/data/saves, then the newest save
//   6. instantiate /engine/game.wasm    -> via instantiateStreaming
//   7. heartbeat every 5 s visible / 30 s hidden
//   8. a keepalive goodbye as the page goes away
//
// Deliberate choices, each of which the fixture is meant to test:
//
//   * No browser storage is authoritative. sessionStorage holds only the
//     session id, so a reload can tell whether it is still the writer; losing
//     it loses nothing (FR-API-3). Saves and config come from the launcher.
//   * The CSRF token is read from the non-HttpOnly `kobra_csrf` cookie, because
//     the server's double-submit check compares the cookie AND the header AND
//     the session's own token (internal/session.CheckCSRF). A value held only
//     in JS is not enough, and a stale JS copy would fail closed.
//   * The goodbye uses fetch(..., {keepalive: true}) rather than
//     navigator.sendBeacon(). sendBeacon cannot set the X-Kobra-CSRF header, so
//     it cannot pass the double-submit check on a POST: /__kobra/goodbye would
//     answer 403 and the launcher would fall back to its idle timeout.

import { loadEngine, tick } from '/engine/game.js';

const ENDPOINTS = {
  session: '/__kobra/session',
  heartbeat: '/__kobra/heartbeat',
  goodbye: '/__kobra/goodbye',
  state: '/api/state',
  saves: '/api/data/saves',
  save: '/api/save',
  config: '/api/data/config',
  configWrite: '/api/config',
};

const API_VERSION = 1;
// FR-SRV-18 defaults. readState() replaces these with the launcher's
// server.heartbeat_interval_seconds as soon as /api/state answers; the
// literal 5/30 pair is only what a shell uses before that and if the launcher
// does not publish the value.
let heartbeatVisibleMs = 5000;
let heartbeatHiddenMs = 30000;
const PLAYER = 'pkgtest-player';

const session = {
  id: '',
  csrf: '',
  writer: null,
  writable: false,
  readOnly: false,
  engine: null,
  engineNote: '',
  frame: 0,
  tickValue: 0,
  slot: 'slot1',
  revision: 0,
  save: null,
  config: null,
  configRevision: 0,
  apiVersion: 0,
  release: '',
  gameId: '',
  engineVersion: '',
  saveVersion: 0,
  dataDirKind: '',
};

const el = {};

function cacheElements() {
  for (const id of [
    'game-id', 'origin', 'banner', 'engine-status', 'save-status', 'settings-status',
    'slot', 'volume', 'volume-out', 'frame', 'log',
    'fact-launcher', 'fact-release', 'fact-engine', 'fact-save-version',
    'fact-writable', 'fact-datadir', 'fact-session',
  ]) {
    el[id] = document.getElementById(id);
  }
  el.newBtn = document.getElementById('btn-new');
  el.saveBtn = document.getElementById('btn-save');
  el.loadBtn = document.getElementById('btn-load');
  el.applyBtn = document.getElementById('btn-apply');
}

// --- small helpers ---------------------------------------------------------

function log(level, message) {
  const li = document.createElement('li');
  li.dataset.level = level;
  const stamp = new Date().toISOString().slice(11, 19);
  li.textContent = `${stamp}  ${message}`;
  el.log.appendChild(li);
  el.log.scrollTop = el.log.scrollHeight;
  while (el.log.childElementCount > 200) el.log.removeChild(el.log.firstChild);
}

function setBanner(kind, text) {
  if (!text) { el.banner.hidden = true; return; }
  el.banner.hidden = false;
  el.banner.dataset.kind = kind;
  el.banner.textContent = text;
}

function readCookie(name) {
  for (const part of document.cookie.split(';')) {
    const [k, ...rest] = part.trim().split('=');
    if (k === name) return decodeURIComponent(rest.join('='));
  }
  return '';
}

function setText(node, value) {
  node.textContent = value === undefined || value === null || value === '' ? '—' : String(value);
}

/** Fetch an API path with the headers every request must carry. */
async function api(path, { method = 'GET', body, keepalive = false } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (session.csrf) headers['X-Kobra-CSRF'] = session.csrf;

  const response = await fetch(path, {
    method,
    headers,
    keepalive,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (response.status === 204) return null;

  const text = await response.text();
  let parsed = null;
  if (text) {
    try { parsed = JSON.parse(text); } catch { parsed = null; }
  }

  if (!response.ok) {
    const error = new Error((parsed && parsed.message) || `HTTP ${response.status}`);
    error.status = response.status;
    error.envelope = parsed;
    throw error;
  }
  return parsed;
}

// --- boot ------------------------------------------------------------------

async function boot() {
  cacheElements();
  setText(el['game-id'], 'connecting');
  setText(el['origin'], location.origin);
  wireEvents();

  try {
    await establishSession();
    await readState();
    await readConfig();
    await readSaves();
    await startEngine();
    startHeartbeat();
    log('ok', 'boot complete');
  } catch (error) {
    setBanner('error', error.message);
    log('error', `boot failed: ${error.message}`);
  }
}

/** Exchange the single-use bootstrap token for a session and CSRF token. */
async function establishSession() {
  const fragment = location.hash.startsWith('#') ? location.hash.slice(1) : location.hash;
  const params = new URLSearchParams(fragment);
  const token = params.get('t');

  if (token) {
    // The token is single-use and expires after 120 s (FR-SRV-7), so it is
    // exchanged before anything else and the fragment is erased immediately.
    const result = await api(ENDPOINTS.session, { method: 'POST', body: { token } });
    session.id = result.session_id;
    session.apiVersion = result.api_version;
    history.replaceState(null, '', location.pathname + location.search);
    log('ok', `session established (api_version ${result.api_version})`);
  } else if (readCookie('kobra_session')) {
    // A reload: the session cookie is still valid, so no token is needed.
    session.id = sessionStorage.getItem('kobra.session_id') || '';
    log('warn', 'no bootstrap token in the fragment; reusing the existing session cookie');
  } else {
    setBanner('warn', 'No session. Start the game from the launcher so it can open this page with a token.');
    throw new Error('no session and no bootstrap token');
  }

  // The double-submit token must match the cookie the server set, so the cookie
  // is the source of truth; the exchange response is only a fallback.
  session.csrf = readCookie('kobra_csrf');
  if (!session.csrf) {
    throw new Error('the CSRF cookie was not set; writes would fail closed');
  }
  if (session.id) sessionStorage.setItem('kobra.session_id', session.id);
}

async function readState() {
  const state = await api(ENDPOINTS.state);

  if (state.api_version !== API_VERSION) {
    throw new Error(`The launcher speaks data API ${state.api_version}; this game needs ${API_VERSION}. Update the launcher.`);
  }

  session.apiVersion = state.api_version;
  session.gameId = state.game_id;
  session.release = state.release;
  session.engineVersion = state.engine_version;
  session.saveVersion = state.save_version;
  session.writable = state.data_writable;
  session.dataDirKind = state.data_dir_kind || 'game';
  session.writer = state.writer ?? null;
  // FR-SRV-18: the cadence is the launcher's to configure. Six times the
  // visible interval is the documented hidden-page value.
  if (Number.isInteger(state.heartbeat_interval_seconds) && state.heartbeat_interval_seconds > 0) {
    heartbeatVisibleMs = state.heartbeat_interval_seconds * 1000;
    heartbeatHiddenMs = heartbeatVisibleMs * 6;
  }

  setText(el['game-id'], state.game_id);
  setText(el['fact-launcher'], `data API v${state.api_version}`);
  setText(el['fact-release'], state.release);
  setText(el['fact-engine'], state.engine_version);
  setText(el['fact-save-version'], state.save_version);
  setText(el['fact-writable'], state.data_writable ? 'yes' : 'no');
  setText(el['fact-datadir'], state.data_dir_kind || 'game');
  setText(el['fact-session'], session.id ? session.id.slice(0, 8) : 'cookie');

  if (!state.data_writable) {
    // FR-SHELL-3 / FR-SAVE-18: read-only media degrades to an in-memory
    // session with a persistent banner, never a silent failure to save.
    setBanner('warn', 'This device is read-only. Saves are held in memory and will be lost when the window closes.');
    log('warn', 'data_writable is false: running an in-memory session');
  }

  if (session.writer && session.id && session.writer !== session.id) {
    session.readOnly = true;
    setBanner('warn', "Another window is playing. This window won't save.");
    log('warn', `writer is held by another session (${String(session.writer).slice(0, 8)})`);
  }

  log('ok', `state: release ${state.release}, engine ${state.engine_version}, save_version ${state.save_version}`);
}

async function readConfig() {
  try {
    session.config = await api(ENDPOINTS.config);
    session.configRevision = session.config.revision || 0;
    const volume = session.config.audio && typeof session.config.audio.volume === 'number'
      ? session.config.audio.volume : 0.8;
    el.volume.value = String(Math.round(volume * 100));
    el['volume-out'].textContent = volume.toFixed(2);
    setText(el['settings-status'], 'Loaded from data/config/settings.json.');
    log('ok', 'config loaded');
  } catch (error) {
    if (error.status === 404) {
      // First run: no settings.json yet. The shell owns the default and writes
      // it once so the user has a real file to edit (FS §10.1 step 6).
      session.config = { audio: { volume: 0.8 }, locale: 'en' };
      setText(el['settings-status'], 'No settings file yet; defaults in memory.');
      log('warn', 'no config on disk; using defaults');
      return;
    }
    throw error;
  }
}

async function readSaves() {
  const list = await api(ENDPOINTS.saves);
  const saves = list.saves || [];
  if (list.rebuilt_index) log('warn', 'the save index was missing and has been rebuilt');

  if (saves.length === 0) {
    setText(el['save-status'], 'No saves yet.');
    log('ok', 'save index is empty');
    return;
  }

  // Newest by modified, falling back to the highest revision.
  const newest = saves.slice().sort((a, b) => {
    const at = Date.parse(a.modified || 0) || 0;
    const bt = Date.parse(b.modified || 0) || 0;
    return bt - at || (b.revision || 0) - (a.revision || 0);
  })[0];

  log('ok', `save index: ${saves.length} slot(s); newest is ${newest.slot} at revision ${newest.revision}`);
  await loadSave(newest.slot, { quiet: true });
}

async function startEngine() {
  const engine = await loadEngine();
  session.engine = engine.exports;
  session.tickValue = tick(engine.exports, 0);
  session.engineNote = engine.streaming
    ? `streaming (${engine.mime}, ${engine.bytes} bytes)`
    : `buffered (${engine.mime})`;
  el['engine-status'].textContent = engine.streaming
    ? 'Engine running from application/wasm.'
    : 'Engine running from a buffer — wrong media type.';
  el['engine-status'].dataset.kind = engine.streaming ? 'ok' : 'warn';
  log(engine.streaming ? 'ok' : 'warn', `engine instantiated ${session.engineNote}`);
  requestAnimationFrame(renderFrame);
}

function renderFrame() {
  const canvas = el.frame;
  const ctx = canvas.getContext('2d');
  const size = canvas.width;
  session.frame += 1;
  session.tickValue = tick(session.engine, session.frame);

  const image = ctx.createImageData(size, size);
  const phase = session.tickValue % 4;
  for (let y = 0; y < size; y += 1) {
    for (let x = 0; x < size; x += 1) {
      const i = (y * size + x) * 4;
      const on = (x + y + phase) % 4 === 0;
      image.data[i] = on ? 99 : 22;
      image.data[i + 1] = on ? 179 : 24;
      image.data[i + 2] = on ? 237 : 29;
      image.data[i + 3] = 255;
    }
  }
  ctx.putImageData(image, 0, 0);
  // 10 fps is plenty to show the engine is really running; a fixture that spins
  // at 60 fps just burns a core while a human reads the diagnostics pane.
  setTimeout(renderFrame, 100);
}

// --- lifecycle -------------------------------------------------------------

let heartbeatTimer = null;

function startHeartbeat() {
  scheduleHeartbeat();
  document.addEventListener('visibilitychange', () => {
    if (heartbeatTimer) clearTimeout(heartbeatTimer);
    scheduleHeartbeat();
  });
  // /__kobra/goodbye is a POST and therefore CSRF-gated. fetch(keepalive) can
  // still set the header, which sendBeacon cannot.
  window.addEventListener('pagehide', () => {
    if (heartbeatTimer) clearTimeout(heartbeatTimer);
    api(ENDPOINTS.goodbye, { method: 'POST', keepalive: true }).catch(() => {});
  });
}

function scheduleHeartbeat() {
  const delay = document.visibilityState === 'hidden' ? heartbeatHiddenMs : heartbeatVisibleMs;
  heartbeatTimer = setTimeout(async () => {
    try {
      await api(ENDPOINTS.heartbeat, { method: 'POST' });
    } catch (error) {
      log('warn', `heartbeat failed: ${error.message}`);
    }
    scheduleHeartbeat();
  }, delay);
}

// --- game actions ----------------------------------------------------------

function currentPayload() {
  return {
    levelId: 'sandbox',
    player: PLAYER,
    position: [session.frame % 8, session.tickValue % 8],
    tick: session.tickValue,
    inventory: session.save && session.save.payload ? session.save.payload.inventory || [] : [],
  };
}

async function saveGame({ ifRevision, slot } = {}) {
  const target = slot || el.slot.value.trim() || session.slot;
  const body = {
    slot: target,
    payload: currentPayload(),
    if_revision: ifRevision === undefined ? session.revision : ifRevision,
    claim: session.revision === 0,
    meta: { playtime_seconds: Math.round(performance.now() / 1000), title: 'Package test' },
  };

  const result = await api(ENDPOINTS.save, { method: 'POST', body });
  session.slot = result.slot;
  session.revision = result.revision;
  el.slot.value = result.slot;
  session.save = { payload: body.payload, revision: result.revision, modified: result.modified };
  setText(el['save-status'], `Saved ${result.slot} at revision ${result.revision} (${result.bytes} bytes).`);
  log('ok', `save wrote ${result.slot} revision ${result.revision}`);
  return result;
}

async function handleSave() {
  if (session.readOnly || !session.writable) return;
  try {
    await saveGame();
  } catch (error) {
    if (error.status === 409 && error.envelope && error.envelope.current_revision !== undefined) {
      const current = error.envelope.current_revision;
      setBanner('warn', `That slot changed underneath you (now revision ${current}). Choose how to continue.`);
      log('warn', `save conflict: server revision ${current}, ours ${session.revision}`);
      // FR-SAVE-10: overwrite / save-as-new-slot / cancel, defaulting to a new slot.
      const overwrite = window.confirm(
        `Slot "${session.slot}" is at revision ${current}; this window last saw ${session.revision}.\n\n` +
        'OK = overwrite it, Cancel = save into a new slot instead.'
      );
      if (overwrite) {
        await saveGame({ ifRevision: current });
      } else {
        await saveGame({ ifRevision: 0, slot: `${session.slot}-copy` });
      }
      setBanner('', '');
      return;
    }
    throw error;
  }
  setBanner('', '');
}

async function loadSave(slot, { quiet = false } = {}) {
  const document_ = await api(`${ENDPOINTS.saves}/${encodeURIComponent(slot)}`);
  session.slot = slot;
  session.revision = document_.revision || 0;
  session.save = document_;
  el.slot.value = slot;
  if (!quiet) {
    setText(el['save-status'], `Loaded ${slot} at revision ${session.revision}.`);
    log('ok', `save loaded ${slot} revision ${session.revision}`);
  }
  return document_;
}

async function handleLoad() {
  const slot = el.slot.value.trim() || session.slot;
  try {
    await loadSave(slot);
  } catch (error) {
    setText(el['save-status'], error.status === 404 ? `No save in slot ${slot}.` : error.message);
    log(error.status === 404 ? 'warn' : 'error', `load failed: ${error.message}`);
  }
}

async function handleNewGame() {
  session.revision = 0;
  session.frame = 0;
  session.save = null;
  setText(el['save-status'], 'New game started. Nothing saved yet.');
  log('ok', 'new game: in-memory state reset');
}

async function handleApplySettings() {
  if (session.readOnly || !session.writable) return;
  const volume = Number(el.volume.value) / 100;
  try {
    const result = await api(ENDPOINTS.configWrite, {
      method: 'POST',
      body: { merge: { audio: { volume } }, if_revision: session.configRevision },
    });
    session.configRevision = result.revision;
    setText(el['settings-status'], `Written at revision ${result.revision}.`);
    log('ok', `config merge wrote revision ${result.revision}`);
  } catch (error) {
    if (error.status === 409) {
      setText(el['settings-status'], 'Settings changed elsewhere; reload the page and try again.');
    } else {
      setText(el['settings-status'], error.message);
    }
    log('error', `config write failed: ${error.message}`);
  }
}

function wireEvents() {
  el.newBtn.addEventListener('click', () => handleNewGame().catch((e) => log('error', e.message)));
  el.saveBtn.addEventListener('click', () => handleSave().catch((e) => log('error', `save failed: ${e.message}`)));
  el.loadBtn.addEventListener('click', () => handleLoad().catch((e) => log('error', e.message)));
  el.applyBtn.addEventListener('click', () => handleApplySettings().catch((e) => log('error', e.message)));
  el.volume.addEventListener('input', () => {
    el['volume-out'].textContent = (Number(el.volume.value) / 100).toFixed(2);
  });
}

function applyReadOnlyMode() {
  if (!session.readOnly && session.writable) return;
  el.saveBtn.disabled = true;
  el.applyBtn.disabled = true;
  el.newBtn.disabled = !session.writable;
}

boot().then(applyReadOnlyMode).catch((error) => {
  log('error', `fatal: ${error.message}`);
});
