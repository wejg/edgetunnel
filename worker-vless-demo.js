/**
 * Worker: 通用 API + VLESS over WebSocket 代理
 *
 * 功能：CORS、REST 路由（/info、/api/*、/ping 等）、VLESS WebSocket 入站与 TCP 出站转发。
 * VLESS 连不上时请检查：1) 域名已绑定到本 Worker；2) 路由匹配 path（如 / 或 *domain/*）。
 */
import { connect } from 'cloudflare:sockets';

// ---------------------------------------------------------------------------
// 日志：统一格式 [时间] [级别] [组件] 消息，便于排查与采集
// ---------------------------------------------------------------------------
const LOG_LEVEL = { INFO: 'INFO', WARN: 'WARN', ERROR: 'ERROR' };
const COMPONENT = { MAIN: 'main', VLESS: 'vless', WS: 'ws' };

function log(level, component, message, detail = null) {
  const ts = new Date().toISOString();
  let payload = message;
  if (detail != null) {
    try {
      payload += ` ${JSON.stringify(detail)}`;
    } catch (_) {
      payload += ` ${String(detail)}`;
    }
  }
  const line = `[${ts}] [${level}] [${component}] ${payload}`;
  if (level === LOG_LEVEL.ERROR) console.error(line);
  else console.log(line);
}

// ---------------------------------------------------------------------------
// CORS 与 JSON 响应
// ---------------------------------------------------------------------------
const CORS_HEADERS = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
  'Access-Control-Allow-Headers': 'Content-Type, Authorization',
};

/**
 * 返回 JSON 响应，附带 CORS 头
 * @param {object} data - 响应体对象
 * @param {object} [extra={}] - 额外响应头
 * @param {number} [status=200] - HTTP 状态码
 */
function jsonResponse(data, extra = {}, status = 200) {
  let body;
  try {
    body = JSON.stringify(data);
  } catch (_) {
    body = '{"error":"Internal Server Error","message":"JSON serialization failed"}';
    status = 500;
  }
  return new Response(body, {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8', ...CORS_HEADERS, ...extra },
  });
}

/** 默认 UUID（环境未设置 UUID/uuid 时使用） */
const DEFAULT_UUID = 'a8f59679-fa3d-4759-8913-c314a949714e';

/**
 * 安全关闭 WebSocket 或 TCP Socket（readyState 1=OPEN 2=CLOSING）
 */
function safeClose(socket) {
  try {
    if (socket && (socket.readyState === 1 || socket.readyState === 2)) socket.close();
  } catch (_) {}
}

/**
 * 将 16 字节转为 UUID 字符串（xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx）
 * @param {Uint8Array|ArrayBuffer} arr - 至少 16 字节
 * @param {number} [offset=0] - 起始偏移
 */
function bytesToUUID(arr, offset = 0) {
  const a = arr instanceof Uint8Array ? arr : new Uint8Array(arr);
  const h = [...a.slice(offset, offset + 16)].map(b => b.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/**
 * 解析 VLESS 首包：校验 UUID、取出版本/命令/目标地址与端口、剩余数据起始下标。
 * 协议：1B ver + 16B UUID + 1B optLen + opts + 1B cmd + 2B port + 1B atype + 变长 address。
 * @param {ArrayBuffer} buf - 首包数据
 * @param {string} token - 期望的 UUID（小写）
 * @returns {{ err: boolean, msg?: string, port?: number, host?: string, udp?: boolean, ri?: number, ver?: Uint8Array }}
 */
function parseVLESSHeader(buf, token) {
  const minFixed = 1 + 16 + 1; // ver + uuid + optLen
  if (buf.byteLength < minFixed) return { err: true, msg: 'bad' };
  const ver = new Uint8Array(buf.slice(0, 1));
  if (bytesToUUID(new Uint8Array(buf.slice(1, 17))) !== token) return { err: true, msg: 'bad' };
  const optLen = new Uint8Array(buf.slice(17, 18))[0];
  const cmdPortAtypeStart = 18 + optLen; // cmd(1) + port(2) + atype(1) = 4
  if (buf.byteLength < cmdPortAtypeStart + 4) return { err: true, msg: 'bad' };
  const cmd = new Uint8Array(buf.slice(cmdPortAtypeStart, cmdPortAtypeStart + 1))[0];
  let udp = false;
  if (cmd === 1) { /* TCP */ } else if (cmd === 2) udp = true; else return { err: true, msg: 'bad' };
  const portStart = cmdPortAtypeStart + 1;
  const port = new DataView(buf.slice(portStart, portStart + 2)).getUint16(0);
  let addrStart = portStart + 2, addrLen = 0, addrValueStart = addrStart + 1;
  let host = '';
  const atype = new Uint8Array(buf.slice(addrStart, addrValueStart))[0];
  if (atype === 1) {
    addrLen = 4;
    if (buf.byteLength < addrValueStart + addrLen) return { err: true, msg: 'bad' };
    host = new Uint8Array(buf.slice(addrValueStart, addrValueStart + addrLen)).join('.');
  } else if (atype === 2) {
    if (buf.byteLength < addrValueStart + 1) return { err: true, msg: 'bad' };
    addrLen = new Uint8Array(buf.slice(addrValueStart, addrValueStart + 1))[0];
    addrValueStart += 1;
    if (buf.byteLength < addrValueStart + addrLen) return { err: true, msg: 'bad' };
    host = new TextDecoder().decode(buf.slice(addrValueStart, addrValueStart + addrLen));
  } else if (atype === 3) {
    addrLen = 16;
    if (buf.byteLength < addrValueStart + addrLen) return { err: true, msg: 'bad' };
    const v6 = [];
    const view = new DataView(buf.slice(addrValueStart, addrValueStart + addrLen));
    for (let i = 0; i < 8; i++) v6.push(view.getUint16(i * 2).toString(16));
    host = v6.join(':');
  } else return { err: true, msg: 'bad' };
  if (!host) return { err: true, msg: 'bad' };
  return { err: false, port, host, udp, ri: addrValueStart + addrLen, ver };
}

/**
 * Base64 解码为 ArrayBuffer（支持 URL-safe 替换 -/_）
 */
function base64Decode(str) {
  if (!str) return { data: null, e: null };
  try {
    const bin = atob(str.replace(/-/g, '+').replace(/_/g, '/'));
    const arr = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
    return { data: arr.buffer, e: null };
  } catch (e) { return { data: null, e }; }
}

/**
 * 将 WebSocket 服务端 + 可选的 early-data（sec-websocket-protocol）转为 ReadableStream
 */
function wsToReadable(serverSocket, earlyDataBase64) {
  let cancelled = false;
  return new ReadableStream({
    start(ctrl) {
      serverSocket.addEventListener('message', e => { if (!cancelled) ctrl.enqueue(e.data); });
      serverSocket.addEventListener('close', () => { if (!cancelled) { safeClose(serverSocket); ctrl.close(); } });
      serverSocket.addEventListener('error', err => ctrl.error(err));
      const { data, e } = base64Decode(earlyDataBase64);
      if (!e && data) ctrl.enqueue(data);
    },
    cancel() { cancelled = true; safeClose(serverSocket); }
  });
}

/**
 * 将远端 TCP readable 管道到 WebSocket，首块可带 VLESS 响应头 hd
 */
function pipeRemoteToWS(remoteSocket, ws, firstChunkHeader) {
  let hd = firstChunkHeader;
  return remoteSocket.readable.pipeTo(new WritableStream({
    async write(chunk) {
      if (ws.readyState !== 1) return;
      if (hd) {
        const combined = new Uint8Array(hd.length + chunk.byteLength);
        combined.set(hd, 0);
        combined.set(new Uint8Array(chunk), hd.length);
        ws.send(combined.buffer);
        hd = null;
      } else ws.send(chunk);
    }
  })).catch(() => safeClose(ws));
}

/**
 * 建立到 target host:port 的 TCP 连接，写入首包 data，并将远端 readable 转发到 ws；
 * 响应首块带 VLESS 头 hd。连接信息存入 box.s，关闭时顺带关闭 ws。
 * 连接超时或写入失败时会关闭 rem 并重新抛出错误。
 */
async function connectTCPAndPipe(host, port, data, ws, hd, box) {
  const rem = connect({ hostname: host, port });
  try {
    await Promise.race([
      rem.opened,
      new Promise((_, rej) => setTimeout(() => rej(new Error('open timeout')), 5000))
    ]);
    const w = rem.writable.getWriter();
    await w.write(data);
    w.releaseLock();
  } catch (e) {
    safeClose(rem);
    throw e;
  }
  box.s = rem;
  rem.closed.catch(() => {}).finally(() => safeClose(ws));
  pipeRemoteToWS(rem, ws, hd);
  log(LOG_LEVEL.INFO, COMPONENT.VLESS, 'TCP 已连接并转发', { host, port });
}

/**
 * UDP 仅支持目标端口 53（DNS）：通过 TCP 连 8.8.8.8:53 转发首包并 pipe 回 WS
 */
async function handleUDPDNS(chunk, ws, header) {
  try {
    const t = connect({ hostname: '8.8.8.8', port: 53 });
    let hd = header;
    const w = t.writable.getWriter();
    await w.write(chunk);
    w.releaseLock();
    t.readable.pipeTo(new WritableStream({
      async write(c) {
        if (ws.readyState !== 1) return;
        if (hd) {
          const r = new Uint8Array(hd.length + c.byteLength);
          r.set(hd, 0); r.set(new Uint8Array(c), hd.length);
          ws.send(r.buffer);
          hd = null;
        } else ws.send(c);
      }
    })).catch((e) => {
      safeClose(ws);
      log(LOG_LEVEL.WARN, COMPONENT.VLESS, 'UDP DNS 管道关闭', { message: e?.message });
    });
    log(LOG_LEVEL.INFO, COMPONENT.VLESS, 'UDP DNS 已转发');
  } catch (e) {
    log(LOG_LEVEL.ERROR, COMPONENT.VLESS, 'UDP DNS 转发失败', { message: e?.message });
  }
}

/**
 * 处理 VLESS WebSocket 请求：接受 WS，从首包解析目标，建立 TCP 或 UDP(DNS) 转发
 */
async function handleVLESSWebSocket(req, uid) {
  const token = uid.toLowerCase();
  const [clientWS, serverWS] = Object.values(new WebSocketPair());
  serverWS.accept();
  const box = { s: null };
  let dnsMode = false;
  const earlyData = req.headers.get('sec-websocket-protocol') || '';
  const readable = wsToReadable(serverWS, earlyData);
  readable.pipeTo(new WritableStream({
    async write(chunk) {
      if (dnsMode) return handleUDPDNS(chunk, serverWS, null);
      if (box.s) {
        const w = box.s.writable.getWriter();
        await w.write(chunk);
        w.releaseLock();
        return;
      }
      const parsed = parseVLESSHeader(chunk, token);
      if (parsed.err) {
        log(LOG_LEVEL.WARN, COMPONENT.VLESS, 'VLESS 首包解析失败', { msg: parsed.msg });
        throw new Error(parsed.msg);
      }
      const raw = chunk.slice(parsed.ri);
      const responseHeader = new Uint8Array([parsed.ver[0], 0]);
      if (parsed.udp) {
        if (parsed.port === 53) { dnsMode = true; return handleUDPDNS(raw, serverWS, responseHeader); }
        log(LOG_LEVEL.WARN, COMPONENT.VLESS, '不支持的 UDP 目标', { port: parsed.port });
        throw new Error('unsupported');
      }
      await connectTCPAndPipe(parsed.host, parsed.port, raw, serverWS, responseHeader, box);
    }
  })).catch((err) => {
    log(LOG_LEVEL.ERROR, COMPONENT.WS, 'VLESS 管道异常', { message: err?.message });
    safeClose(serverWS);
  });
  log(LOG_LEVEL.INFO, COMPONENT.WS, 'WebSocket 已接受（VLESS）');
  return new Response(null, { status: 101, webSocket: clientWS });
}

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const path = url.pathname;
    const method = request.method;
    log(LOG_LEVEL.INFO, COMPONENT.MAIN, '请求', { method, path });

    if (method === 'OPTIONS') {
      return new Response(null, { headers: CORS_HEADERS });
    }

    const upgrade = request.headers.get('Upgrade');
    if (upgrade === 'websocket') {
      const uid = (env.UUID || env.uuid || DEFAULT_UUID).trim().toLowerCase();
      const uuidRe = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
      if (!uuidRe.test(uid)) {
        log(LOG_LEVEL.WARN, COMPONENT.MAIN, 'WebSocket 请求 UUID 格式无效', { uid: uid.slice(0, 8) + '...' });
        return jsonResponse({ error: 'bad request' }, {}, 400);
      }
      return handleVLESSWebSocket(request, uid);
    }

    try {
      if (path === '/' || path === '') {
        return jsonResponse({ message: 'Hello World!', timestamp: new Date().toISOString() });
      }
      if (path === '/info') {
        const headers = Object.fromEntries(request.headers.entries());
        const safeHeaders = { ...headers };
        if (safeHeaders.cookie) safeHeaders.cookie = '[redacted]';
        if (safeHeaders.authorization) safeHeaders.authorization = '[redacted]';
        return jsonResponse({
          method,
          url: url.href,
          path,
          headers: safeHeaders,
          cf: request.cf,
        });
      }
      if (path === '/env') {
        return jsonResponse({ hasSecret: !!env.MY_SECRET });
      }
      if (path === '/echo' && method === 'POST') {
        const body = await request.text();
        return jsonResponse({ echoed: body, length: body.length });
      }
      if (path === '/api/status') {
        return jsonResponse({ status: 'ok', uptime: Date.now() % 1e6 });
      }
      if (path === '/api/health') {
        return jsonResponse({ healthy: true, ts: new Date().toISOString() });
      }
      if (path === '/api/version') {
        return jsonResponse({ version: '1.0.0', build: '20250101' });
      }
      if (path === '/api/config') {
        return jsonResponse({ debug: false, features: ['cors', 'json'] });
      }
      if (path === '/api/users') {
        return jsonResponse({ users: [], total: 0 });
      }
      if (path === '/api/metrics') {
        return jsonResponse({ requests: 0, errors: 0 });
      }
      if (path === '/ping') {
        return jsonResponse({ pong: true });
      }
      if (path === '/time') {
        return jsonResponse({ epoch: Date.now(), iso: new Date().toISOString() });
      }
      if (path === '/api/random') {
        return jsonResponse({ value: Math.random(), id: Math.floor(Math.random() * 1e9) });
      }
      if (path === '/api/debug') {
        return jsonResponse({ envKeys: Object.keys(env || {}).length, path, method });
      }
      if (path === '/api/features') {
        return jsonResponse({ list: ['rest', 'cors', 'options'] });
      }
      if (path === '/api/limits') {
        return jsonResponse({ rate: 100, burst: 10 });
      }
      if (path === '/api/status/check') {
        return jsonResponse({ ok: true });
      }
      if (path === '/v1/ping') {
        return jsonResponse({ result: 'pong' });
      }
      if (path === '/v1/healthz') {
        return jsonResponse({ status: 'pass' });
      }
      log(LOG_LEVEL.WARN, COMPONENT.MAIN, '未匹配路由', { path });
      return jsonResponse({ error: 'Not Found', path }, {}, 404);
    } catch (err) {
      const msg = err?.message ?? String(err);
      log(LOG_LEVEL.ERROR, COMPONENT.MAIN, '未处理异常', { message: msg });
      return jsonResponse({ error: 'Internal Server Error', message: msg }, {}, 500);
    }
  },
};
