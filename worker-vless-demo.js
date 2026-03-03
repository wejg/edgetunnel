/**
 * VLESS-only demo Worker
 * 只用域名即可：绑定你的域名到本 Worker，客户端用 VLESS+WS 连该域名即可转发。
 * 环境变量：UUID（可选，不设则用下方默认）
 */
import { connect } from 'cloudflare:sockets';

const DEFAULT_UUID = 'a8f59679-fa3d-4759-8913-c314a949714e';

function closeSocketQuietly(socket) {
    try {
        if (socket.readyState === WebSocket.OPEN || socket.readyState === WebSocket.CLOSING) socket.close();
    } catch (_) {}
}

function formatIdentifier(arr, offset = 0) {
    const hex = [...arr.slice(offset, offset + 16)].map(b => b.toString(16).padStart(2, '0')).join('');
    return `${hex.substring(0, 8)}-${hex.substring(8, 12)}-${hex.substring(12, 16)}-${hex.substring(16, 20)}-${hex.substring(20)}`;
}

function parseVLESS(chunk, token) {
    if (chunk.byteLength < 24) return { hasError: true, message: 'Invalid data' };
    const version = new Uint8Array(chunk.slice(0, 1));
    if (formatIdentifier(new Uint8Array(chunk.slice(1, 17))) !== token) return { hasError: true, message: 'Invalid uuid' };
    const optLen = new Uint8Array(chunk.slice(17, 18))[0];
    const cmd = new Uint8Array(chunk.slice(18 + optLen, 19 + optLen))[0];
    let isUDP = false;
    if (cmd === 1) {} else if (cmd === 2) { isUDP = true; } else { return { hasError: true, message: 'Invalid command' }; }
    const portIdx = 19 + optLen;
    const port = new DataView(chunk.slice(portIdx, portIdx + 2)).getUint16(0);
    let addrIdx = portIdx + 2, addrLen = 0, addrValIdx = addrIdx + 1, hostname = '';
    const addressType = new Uint8Array(chunk.slice(addrIdx, addrValIdx))[0];
    switch (addressType) {
        case 1:
            addrLen = 4;
            hostname = new Uint8Array(chunk.slice(addrValIdx, addrValIdx + addrLen)).join('.');
            break;
        case 2:
            addrLen = new Uint8Array(chunk.slice(addrValIdx, addrValIdx + 1))[0];
            addrValIdx += 1;
            hostname = new TextDecoder().decode(chunk.slice(addrValIdx, addrValIdx + addrLen));
            break;
        case 3:
            addrLen = 16;
            const ipv6 = [];
            const ipv6View = new DataView(chunk.slice(addrValIdx, addrValIdx + addrLen));
            for (let i = 0; i < 8; i++) ipv6.push(ipv6View.getUint16(i * 2).toString(16));
            hostname = ipv6.join(':');
            break;
        default:
            return { hasError: true, message: `Invalid address type: ${addressType}` };
    }
    if (!hostname) return { hasError: true, message: `Invalid address: ${addressType}` };
    return { hasError: false, port, hostname, isUDP, rawIndex: addrValIdx + addrLen, version };
}

function base64ToArray(b64Str) {
    if (!b64Str) return { earlyData: null, error: null };
    try {
        const binaryString = atob(b64Str.replace(/-/g, '+').replace(/_/g, '/'));
        const bytes = new Uint8Array(binaryString.length);
        for (let i = 0; i < binaryString.length; i++) bytes[i] = binaryString.charCodeAt(i);
        return { earlyData: bytes.buffer, error: null };
    } catch (e) {
        return { earlyData: null, error: e };
    }
}

function makeReadableStr(socket, earlyDataHeader) {
    let cancelled = false;
    return new ReadableStream({
        start(controller) {
            socket.addEventListener('message', (e) => { if (!cancelled) controller.enqueue(e.data); });
            socket.addEventListener('close', () => { if (!cancelled) { closeSocketQuietly(socket); controller.close(); } });
            socket.addEventListener('error', (err) => controller.error(err));
            const { earlyData, error } = base64ToArray(earlyDataHeader);
            if (error) controller.error(error);
            else if (earlyData) controller.enqueue(earlyData);
        },
        cancel() { cancelled = true; closeSocketQuietly(socket); }
    });
}

function connectStreams(remoteSocket, webSocket, headerData) {
    let header = headerData;
    return remoteSocket.readable.pipeTo(new WritableStream({
        async write(chunk) {
            if (webSocket.readyState !== WebSocket.OPEN) return;
            if (header) {
                const resp = new Uint8Array(header.length + chunk.byteLength);
                resp.set(header, 0);
                resp.set(new Uint8Array(chunk), header.length);
                webSocket.send(resp.buffer);
                header = null;
            } else {
                webSocket.send(chunk);
            }
        }
    })).catch(() => closeSocketQuietly(webSocket));
}

async function forwardTCP(host, portNum, rawData, ws, respHeader, remoteConnWrapper) {
    const remote = connect({ hostname: host, port: portNum });
    const w = remote.writable.getWriter();
    await w.write(rawData);
    w.releaseLock();
    remoteConnWrapper.socket = remote;
    remote.closed.catch(() => {}).finally(() => closeSocketQuietly(ws));
    connectStreams(remote, ws, respHeader);
}

async function forwardUDP(udpChunk, webSocket, respHeader) {
    try {
        const tcp = connect({ hostname: '8.8.8.8', port: 53 });
        let h = respHeader;
        const w = tcp.writable.getWriter();
        await w.write(udpChunk);
        w.releaseLock();
        await tcp.readable.pipeTo(new WritableStream({
            async write(chunk) {
                if (webSocket.readyState !== WebSocket.OPEN) return;
                if (h) {
                    const r = new Uint8Array(h.length + chunk.byteLength);
                    r.set(h, 0); r.set(new Uint8Array(chunk), h.length);
                    webSocket.send(r.buffer);
                    h = null;
                } else webSocket.send(chunk);
            }
        }));
    } catch (_) {}
}

async function handleWS(request, uuid) {
    const tokenStr = uuid.toLowerCase();
    const pair = new WebSocketPair();
    const [clientSock, serverSock] = Object.values(pair);
    serverSock.accept();
    const remote = { socket: null };
    let isDns = false;
    const earlyData = request.headers.get('sec-websocket-protocol') || '';
    const readable = makeReadableStr(serverSock, earlyData);
    readable.pipeTo(new WritableStream({
        async write(chunk) {
            if (isDns) return forwardUDP(chunk, serverSock, null);
            if (remote.socket) {
                const w = remote.socket.writable.getWriter();
                await w.write(chunk);
                w.releaseLock();
                return;
            }
            const parsed = parseVLESS(chunk, tokenStr);
            if (parsed.hasError) throw new Error(parsed.message);
            const { port, hostname, isUDP, rawIndex, version } = parsed;
            const rawData = chunk.slice(rawIndex);
            const respHeader = new Uint8Array([version[0], 0]);
            if (isUDP) {
                if (port === 53) { isDns = true; return forwardUDP(rawData, serverSock, respHeader); }
                throw new Error('UDP not supported except DNS');
            }
            await forwardTCP(hostname, port, rawData, serverSock, respHeader, remote);
        }
    })).catch(() => {});
    return new Response(null, { status: 101, webSocket: clientSock });
}

export default {
    async fetch(request, env, ctx) {
        const url = new URL(request.url);
        if (request.headers.get('Upgrade') !== 'websocket') {
            return new Response('VLESS demo. Use WebSocket (VLESS+WS) to this domain.\nUUID: ' + (env.UUID || DEFAULT_UUID), {
                status: 200,
                headers: { 'Content-Type': 'text/plain; charset=utf-8' }
            });
        }
        const uuid = (env.UUID || env.uuid || DEFAULT_UUID).trim().toLowerCase();
        const uuidRegex = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
        if (!uuidRegex.test(uuid)) {
            return new Response('Invalid UUID in env', { status: 400 });
        }
        return handleWS(request, uuid);
    }
};
