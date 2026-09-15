// Engine glue for the Kobra package test game.
//
// This mirrors the shape of an Emscripten module factory without pulling in the
// Emscripten runtime. The point of the fixture is that the WebAssembly core is
// fetched, MIME-checked and instantiated through exactly the path a real engine
// uses, so FR-AST-2 and the `application/wasm` MIME mapping (FR-SRV-13) are
// exercised on every load rather than assumed.
//
// The module is an ES module because the page runs under
// `script-src 'self' 'wasm-unsafe-eval'` and imports this file.

const WASM_URL = '/engine/game.wasm';

/**
 * Fetch and instantiate the engine core.
 *
 * @param {{signal?: AbortSignal}} [options]
 * @returns {Promise<{exports: WebAssembly.Exports, mime: string, streaming: boolean, bytes: number}>}
 */
export async function loadEngine(options = {}) {
  const signal = options.signal;
  const response = await fetch(WASM_URL, { signal, cache: 'no-store' });
  if (!response.ok) {
    throw new Error(`engine fetch failed: HTTP ${response.status}`);
  }

  const contentType = (response.headers.get('Content-Type') || '').split(';')[0].trim();
  const bytes = Number(response.headers.get('Content-Length') || 0);

  // FR-AST-2: instantiateStreaming is the primary path and requires the exact
  // application/wasm media type. The fallback exists for a misconfigured
  // server, and a wrong media type is a build error that is logged as a
  // diagnostic rather than silently tolerated.
  let instance;
  let streaming = true;
  if (contentType === 'application/wasm') {
    ({ instance } = await WebAssembly.instantiateStreaming(response, {}));
  } else {
    streaming = false;
    console.warn('engine.mime_mismatch', { url: WASM_URL, expected: 'application/wasm', actual: contentType });
    ({ instance } = await WebAssembly.instantiate(await response.arrayBuffer(), {}));
  }

  return { exports: instance.exports, mime: contentType, streaming, bytes };
}

/**
 * The engine's per-frame entry point. The fixture's core advances a counter by
 * one per call so the shell can prove it is really running code on the main
 * thread, not rendering a placeholder.
 */
export function tick(exports, frame) {
  if (typeof exports.tick !== 'function') {
    throw new Error('engine does not export tick()');
  }
  return exports.tick(frame);
}
