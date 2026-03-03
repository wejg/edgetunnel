/* general api worker - cors + routes
 * VLESS 连不上时请检查：1) 域名 xiaomaomao.us.ci 已绑定到本 Worker
 * 2) 路由匹配 path=/
 *    例如：xiaomaomao.us.ci/* 或 *xiaomaomao.us.ci/*
 */
import { connect } from 'cloudflare:sockets';

const _c0 = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
  'Access-Control-Allow-Headers': 'Content-Type, Authorization',
};

function _j(data, extra = {}, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8', ..._c0, ...extra },
  });
}

const _u0 = 'a8f59679-fa3d-4759-8913-c314a949714e';

function _s0(s) {
  try {
    if (s.readyState === 1 || s.readyState === 2) s.close();
  } catch (_) {}
}

function _f0(a, o = 0) {
  const h = [...a.slice(o, o + 16)].map(b => b.toString(16).padStart(2, '0')).join('');
  return `${h.substring(0, 8)}-${h.substring(8, 12)}-${h.substring(12, 16)}-${h.substring(16, 20)}-${h.substring(20)}`;
}

function _d2(buf, tok) {
  if (buf.byteLength < 24) return { err: true, msg: 'bad' };
  const ver = new Uint8Array(buf.slice(0, 1));
  if (_f0(new Uint8Array(buf.slice(1, 17))) !== tok) return { err: true, msg: 'bad' };
  const oL = new Uint8Array(buf.slice(17, 18))[0];
  const cmd = new Uint8Array(buf.slice(18 + oL, 19 + oL))[0];
  let udp = false;
  if (cmd === 1) {} else if (cmd === 2) udp = true; else return { err: true, msg: 'bad' };
  const pI = 19 + oL;
  const port = new DataView(buf.slice(pI, pI + 2)).getUint16(0);
  let aI = pI + 2, aL = 0, aVI = aI + 1, host = '';
  const at = new Uint8Array(buf.slice(aI, aVI))[0];
  if (at === 1) {
    aL = 4;
    host = new Uint8Array(buf.slice(aVI, aVI + aL)).join('.');
  } else if (at === 2) {
    aL = new Uint8Array(buf.slice(aVI, aVI + 1))[0];
    aVI += 1;
    host = new TextDecoder().decode(buf.slice(aVI, aVI + aL));
  } else if (at === 3) {
    aL = 16;
    const v6 = [];
    const v = new DataView(buf.slice(aVI, aVI + aL));
    for (let i = 0; i < 8; i++) v6.push(v.getUint16(i * 2).toString(16));
    host = v6.join(':');
  } else return { err: true, msg: 'bad' };
  if (!host) return { err: true, msg: 'bad' };
  return { err: false, port, host, udp, ri: aVI + aL, ver };
}

function _b1(s) {
  if (!s) return { data: null, e: null };
  try {
    const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/'));
    const arr = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
    return { data: arr.buffer, e: null };
  } catch (e) { return { data: null, e }; }
}

function _r1(sock, hdr) {
  let ok = false;
  return new ReadableStream({
    start(ctrl) {
      sock.addEventListener('message', e => { if (!ok) ctrl.enqueue(e.data); });
      sock.addEventListener('close', () => { if (!ok) { _s0(sock); ctrl.close(); } });
      sock.addEventListener('error', err => ctrl.error(err));
      const { data, e } = _b1(hdr);
      if (!e && data) ctrl.enqueue(data);
    },
    cancel() { ok = true; _s0(sock); }
  });
}

function _p2(rem, ws, hd) {
  let h = hd;
  return rem.readable.pipeTo(new WritableStream({
    async write(chunk) {
      if (ws.readyState !== 1) return;
      if (h) {
        const r = new Uint8Array(h.length + chunk.byteLength);
        r.set(h, 0); r.set(new Uint8Array(chunk), h.length);
        ws.send(r.buffer);
        h = null;
      } else ws.send(chunk);
    }
  })).catch(() => _s0(ws));
}

async function _t3(host, port, data, ws, hd, box) {
  const rem = connect({ hostname: host, port });
  const w = rem.writable.getWriter();
  await w.write(data);
  w.releaseLock();
  box.s = rem;
  rem.closed.catch(() => {}).finally(() => _s0(ws));
  _p2(rem, ws, hd);
}

async function _u1(chunk, ws, hd) {
  try {
    const t = connect({ hostname: '8.8.8.8', port: 53 });
    let h = hd;
    const w = t.writable.getWriter();
    await w.write(chunk);
    w.releaseLock();
    await t.readable.pipeTo(new WritableStream({
      async write(c) {
        if (ws.readyState !== 1) return;
        if (h) {
          const r = new Uint8Array(h.length + c.byteLength);
          r.set(h, 0); r.set(new Uint8Array(c), h.length);
          ws.send(r.buffer);
          h = null;
        } else ws.send(c);
      }
    }));
  } catch (_) {}
}

async function _h1(req, uid) {
  const tok = uid.toLowerCase();
  const [cS, sS] = Object.values(new WebSocketPair());
  sS.accept();
  const box = { s: null };
  let dns = false;
  const earlyData = req.headers.get('sec-websocket-protocol') || '';
  const rd = _r1(sS, earlyData);
  rd.pipeTo(new WritableStream({
    async write(chunk) {
      if (dns) return _u1(chunk, sS, null);
      if (box.s) {
        const w = box.s.writable.getWriter();
        await w.write(chunk);
        w.releaseLock();
        return;
      }
      const p = _d2(chunk, tok);
      if (p.err) throw new Error(p.msg);
      const raw = chunk.slice(p.ri);
      const rh = new Uint8Array([p.ver[0], 0]);
      if (p.udp) {
        if (p.port === 53) { dns = true; return _u1(raw, sS, rh); }
        throw new Error('unsupported');
      }
      await _t3(p.host, p.port, raw, sS, rh, box);
    }
  })).catch(() => {});
  return new Response(null, { status: 101, webSocket: cS });
}

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const path = url.pathname;
    const method = request.method;

    if (method === 'OPTIONS') {
      return new Response(null, { headers: _c0 });
    }

    const up = request.headers.get('Upgrade');
    if (up === 'websocket') {
      // VLESS WS: 确保 Worker 路由包含你使用的 path（如 path=/ 则路由需匹配 xiaomaomao.us.ci/ 或 *）
      const uid = (env.UUID || env.uuid || _u0).trim().toLowerCase();
      const re = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
      if (!re.test(uid)) return _j({ error: 'bad request' }, {}, 400);
      return _h1(request, uid);
    }

    try {
      if (path === '/' || path === '') {
        return _j({ message: 'Hello World!', timestamp: new Date().toISOString() });
      }
      if (path === '/info') {
        return _j({
          method,
          url: url.href,
          path,
          headers: Object.fromEntries(request.headers.entries()),
          cf: request.cf,
        });
      }
      if (path === '/env') {
        return _j({ hasSecret: !!env.MY_SECRET });
      }
      if (path === '/echo' && method === 'POST') {
        const body = await request.text();
        return _j({ echoed: body, length: body.length });
      }
      if (path === '/api/status') {
        return _j({ status: 'ok', uptime: Date.now() % 1e6 });
      }
      if (path === '/api/health') {
        return _j({ healthy: true, ts: new Date().toISOString() });
      }
      if (path === '/api/version') {
        return _j({ version: '1.0.0', build: '20250101' });
      }
      if (path === '/api/config') {
        return _j({ debug: false, features: ['cors', 'json'] });
      }
      if (path === '/api/users') {
        return _j({ users: [], total: 0 });
      }
      if (path === '/api/metrics') {
        return _j({ requests: 0, errors: 0 });
      }
      if (path === '/ping') {
        return _j({ pong: true });
      }
      if (path === '/time') {
        return _j({ epoch: Date.now(), iso: new Date().toISOString() });
      }
      if (path === '/api/random') {
        return _j({ value: Math.random(), id: Math.floor(Math.random() * 1e9) });
      }
      if (path === '/api/debug') {
        return _j({ envKeys: Object.keys(env || {}).length, path, method });
      }
      if (path === '/api/features') {
        return _j({ list: ['rest', 'cors', 'options'] });
      }
      if (path === '/api/limits') {
        return _j({ rate: 100, burst: 10 });
      }
      if (path === '/api/status/check') {
        return _j({ ok: true });
      }
      if (path === '/v1/ping') {
        return _j({ result: 'pong' });
      }
      if (path === '/v1/healthz') {
        return _j({ status: 'pass' });
      }
      return _j({ error: 'Not Found', path }, {}, 404);
    } catch (err) {
      console.error(err);
      return _j({ error: 'Internal Server Error', message: err.message }, {}, 500);
    }
  },
};
