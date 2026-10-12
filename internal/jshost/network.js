(() => {
  'use strict';
  const define = (name, value) => Object.defineProperty(globalThis, name, {
    value, writable: true, configurable: true,
  });
  const identity = __config.browserIdentity || {};
  class Navigator {}
  const nav = {
    userAgent: __config.userAgent || '',
    platform: __config.platform || identity.platform || 'MacIntel',
    language: __config.language || identity.locale || 'zh-CN',
    languages: __config.languages || identity.languages || ['zh-CN', 'zh'],
    hardwareConcurrency: __config.hardwareConcurrency || 8,
    deviceMemory: __config.deviceMemory || 8,
    cookieEnabled: true, onLine: true, vendor: 'Google Inc.', product: 'Gecko',
  };
  nav.appVersion = nav.userAgent.replace(/^Mozilla\//, '');
  for (const [key, value] of Object.entries(nav)) {
    Object.defineProperty(Navigator.prototype, key, {get: () => value, enumerable: true, configurable: true});
  }
  define('Navigator', Navigator);
  define('navigator', new Navigator());
  class Screen {}
  const screen = new Screen();
  Object.assign(screen, {
    width: __config.screenWidth || 1920, height: __config.screenHeight || 1080,
    availWidth: __config.screenWidth || 1920, availHeight: __config.screenHeight || 1080,
    colorDepth: 24, pixelDepth: 24,
  });
  define('Screen', Screen); define('screen', screen);
  define('requestIdleCallback', callback => setTimeout(() => callback({didTimeout: false, timeRemaining: () => 1}), 0));
  define('cancelIdleCallback', clearTimeout);
  define('requestAnimationFrame', callback => setTimeout(() => callback(performance.now()), 16));
  define('cancelAnimationFrame', clearTimeout);
  define('postMessage', (data, options = '/', transfer = []) => __host.postMessage(data, typeof options === 'object' ? options.targetOrigin ?? '/' : options, typeof options === 'object' ? options.transfer || [] : transfer));
  // Deliberately discard remote console output: it may contain credentials or
  // script-controlled URLs. Failures use the host's closed diagnostic codes.
  define('console', Object.fromEntries(['log','info','warn','error','debug','trace','dir','time','timeEnd'].map(name => [name, () => {}])));

  class AbortSignal extends EventTarget {
    constructor() {super(); this.aborted = false; this.reason = undefined;}
    throwIfAborted() {if (this.aborted) throw this.reason;}
    static abort(reason) {const c = new AbortController(); c.abort(reason); return c.signal;}
    static timeout(ms) {const c = new AbortController(); setTimeout(() => c.abort(new DOMException('Operation timed out','TimeoutError')), ms); return c.signal;}
  }
  class AbortController {
    constructor() {this.signal = new AbortSignal();}
    abort(reason = new DOMException('Operation aborted','AbortError')) {
      if (this.signal.aborted) return;
      this.signal.aborted = true; this.signal.reason = reason;
      __domDispatchTrusted(this.signal, new Event('abort'));
    }
  }
  if (typeof globalThis.DOMException === 'undefined') {
    define('DOMException', class DOMException extends Error {constructor(message = '', name = 'Error') {super(message); this.name = name;}});
  }
  define('AbortSignal', AbortSignal); define('AbortController', AbortController);
  const bytesBase64 = bytes => {
    const chunks = [];
    for (let i = 0; i < bytes.length; i += 8192) chunks.push(String.fromCharCode(...bytes.subarray(i, i + 8192)));
    return btoa(chunks.join(''));
  };
  const replyBytes = reply => {
    if (typeof reply.bodyBase64 !== 'string') return new TextEncoder().encode(reply.body || '');
    const binary = atob(reply.bodyBase64), bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes;
  };
  const responseHeaders = reply => {
    const headers = new Headers(reply.headers || {});
    headers.delete('set-cookie'); headers.delete('set-cookie2');
    return headers;
  };
  const request = async (input, init, kind) => {
    const started = performance.now();
    const req = new Request(input, init);
    const signal = init?.signal || req.signal;
    signal?.throwIfAborted();
    const bytes = new Uint8Array(await req.arrayBuffer());
    signal?.throwIfAborted();
    const record = {kind, method: req.method, url: req.url,
      headers: Object.fromEntries(req.headers.entries()), bodyBase64: bytesBase64(bytes)};
    const reply = await new Promise((resolve, reject) => {
      const abort = () => reject(signal.reason);
      signal?.addEventListener('abort', abort, {once: true});
      __host.request(record).then(resolve, reject).finally(() => signal?.removeEventListener('abort', abort));
    });
    performance.__addEntry({name: req.url, entryType: 'resource', initiatorType: kind === 'xhr' ? 'xmlhttprequest' : 'fetch', startTime: started, duration: performance.now() - started, responseStatus: reply.status || 0, decodedBodySize: replyBytes(reply).length});
    return reply;
  };
  define('fetch', async (input, init = {}) => {
    const requestURL = new URL(typeof input === 'string' || input instanceof URL ? input : input.url, location.href).href;
    const reply = await request(input, init, 'fetch');
    if (!reply.status) throw new TypeError('Network request failed');
    const bytes = replyBytes(reply);
    const response = new Response([101, 204, 205, 304].includes(reply.status) ? null : bytes, {status: reply.status, headers: responseHeaders(reply)});
    Object.defineProperties(response, {
      url: {value: reply.url || requestURL, configurable: true},
      redirected: {value: !!reply.url && reply.url !== requestURL, configurable: true},
      type: {value: 'basic', configurable: true},
    });
    return response;
  });

  class XMLHttpRequest extends EventTarget {
    constructor() {
      super(); this.readyState = 0; this.status = 0; this.statusText = '';
      this.responseType = ''; this.response = ''; this.responseURL = '';
      this.timeout = 0; this.withCredentials = false; this.upload = new EventTarget();
      this._headers = new Headers(); this._responseHeaders = new Headers();
      this._text = ''; this._sent = false; this._generation = 0;
    }
    get responseText() {
      if (this.responseType !== '' && this.responseType !== 'text') throw new DOMException('Invalid response type','InvalidStateError');
      return this._text;
    }
    open(method, url, async = true) {
      if (!async) throw new TypeError('JS_API_UNSUPPORTED_SYNCHRONOUS_XHR');
      if (this._controller) this._controller.abort();
      this._generation++; this._sent = false; this._method = String(method).toUpperCase();
      this._url = new URL(String(url), location.href).href;
      this._headers = new Headers(); this._text = ''; this.status = 0;
      this._state(1);
    }
    setRequestHeader(name, value) {
      if (this.readyState !== 1 || this._sent) throw new DOMException('Invalid state','InvalidStateError');
      this._headers.append(name, value);
    }
    getAllResponseHeaders() {
      if (this.readyState < 2) return '';
      return [...this._responseHeaders].filter(([name]) => !/^set-cookie2?$/.test(name)).map(([k,v]) => k + ': ' + v + '\r\n').join('');
    }
    getResponseHeader(name) {return this.readyState < 2 || /^set-cookie2?$/i.test(name) ? null : this._responseHeaders.get(name);}
    overrideMimeType(mime) {
      if (this.readyState >= 3) throw new DOMException('Invalid state','InvalidStateError');
      this._mime = String(mime);
    }
    _event(name) {__domDispatchTrusted(this, new Event(name));}
    _state(value) {this.readyState = value; this._event('readystatechange');}
    send(body = null) {
      if (this.readyState !== 1 || this._sent) throw new DOMException('Invalid state','InvalidStateError');
      this._sent = true; this._controller = new AbortController();
      const generation = this._generation;
      let timer = this.timeout > 0 ? setTimeout(() => {this._timedOut = true; this._controller.abort();}, this.timeout) : null;
      this._event('loadstart');
      request(this._url, {method: this._method, headers: this._headers, body: /^(GET|HEAD)$/.test(this._method) ? null : body, signal: this._controller.signal}, 'xhr').then(reply => {
        if (generation !== this._generation || !this._sent) return;
        clearTimeout(timer); this.status = Number(reply.status); this.responseURL = reply.url || this._url;
        this._responseHeaders = responseHeaders(reply); this._state(2);
        if (generation !== this._generation || !this._sent) return;
        const bytes = replyBytes(reply); this._state(3);
        if (generation !== this._generation || !this._sent) return;
        const text = new TextDecoder().decode(bytes);
        if (this.responseType === 'arraybuffer') this.response = bytes.buffer;
        else if (this.responseType === 'blob') this.response = new Blob([bytes], {type: this._mime || this.getResponseHeader('content-type') || ''});
        else if (this.responseType === 'json') {try {this.response = JSON.parse(text);} catch {this.response = null;}}
        else if (this.responseType === '' || this.responseType === 'text') this.response = this._text = text;
        else throw new TypeError('JS_API_UNSUPPORTED_XHR_RESPONSE');
        this._sent = false; this._state(4); this._event('load'); this._event('loadend');
      }).catch(error => {
        clearTimeout(timer);
        if (generation !== this._generation || !this._sent) return;
        this._sent = false; this.status = 0; this._text = ''; this.response = null;
        this._state(4); this._event(this._timedOut ? 'timeout' : this._controller.signal.aborted ? 'abort' : 'error'); this._event('loadend');
      });
    }
    abort() {
      if (this._sent) {
        this._sent = false; this._generation++; this._controller.abort();
        this.status = 0; this._text = ''; this.response = null;
        this._state(4); this._event('abort'); this._event('loadend');
      }
      this.readyState = 0;
    }
  }
  for (const [key, value] of Object.entries({UNSENT:0, OPENED:1, HEADERS_RECEIVED:2, LOADING:3, DONE:4})) {
    Object.defineProperty(XMLHttpRequest, key, {value}); Object.defineProperty(XMLHttpRequest.prototype, key, {value});
  }
  define('XMLHttpRequest', XMLHttpRequest);
  define('__callSDK', (method, flow) => {
    Promise.resolve().then(() => {
      if (typeof SentinelSDK?.[method] !== 'function') throw new TypeError('SDK method unavailable');
      return SentinelSDK[method](flow);
    }).then(value => __host.result(method, flow, value), __host.failure);
  });
})();
