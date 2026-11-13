// Test-only server for the runtime's own WebSocket client. Never used by the adapter.
import { createHash } from 'node:crypto';

function frame(text, opcode = 1) {
  const body = Buffer.isBuffer(text) ? text : Buffer.from(text);
  if (body.length >= 126) throw new Error('fake frames must be short');
  return Buffer.concat([Buffer.from([0x80 | opcode, body.length]), body]);
}

export function attachWakeServer(server, gateway, now) {
  const link = { enabled: true, answerPings: true, attempts: [], sockets: new Set(), pings: [] };
  server.on('upgrade', (req, socket, head) => {
    link.attempts.push({ at: now(), url: req.url, authorization: req.headers.authorization, protocol: req.headers['sec-websocket-protocol'] });
    if (!link.enabled || req.url !== '/v1/wake') {
      socket.end('HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n');
      return;
    }
    if (gateway.revoked || req.headers.authorization !== `Bearer ${gateway.credential}`) {
      socket.end('HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n');
      return;
    }
    const accept = createHash('sha1').update(`${req.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
    link.sockets.add(socket);
    socket.on('close', () => link.sockets.delete(socket));
    socket.on('error', () => {});
    let buffer = head;
    const parse = (chunk) => {
      buffer = Buffer.concat([buffer, chunk]);
      while (buffer.length >= 2) {
        const opcode = buffer[0] & 15;
        const masked = Boolean(buffer[1] & 128);
        let size = buffer[1] & 127;
        let offset = 2;
        if (size === 126) {
          if (buffer.length < 4) return;
          size = buffer.readUInt16BE(2);
          offset = 4;
        }
        if (size === 127 || !masked) { socket.destroy(); return; }
        if (buffer.length < offset + 4 + size) return;
        const mask = buffer.subarray(offset, offset + 4);
        offset += 4;
        const body = Buffer.from(buffer.subarray(offset, offset + size));
        for (let i = 0; i < size; i++) body[i] ^= mask[i % 4];
        buffer = buffer.subarray(offset + size);
        if (opcode === 8) { socket.end(frame(body, 8)); return; }
        if (opcode === 1 && body.toString() === 'ping') {
          link.pings.push(now());
          if (link.answerPings) socket.write(frame('pong'));
        }
      }
    };
    socket.on('data', parse);
    if (head.length) { buffer = Buffer.alloc(0); parse(head); }
  });
  link.wake = () => { for (const socket of link.sockets) socket.write(frame('wake')); };
  link.close = (code) => {
    const body = Buffer.alloc(2);
    body.writeUInt16BE(code);
    for (const socket of link.sockets) socket.write(frame(body, 8));
  };
  link.drop = () => { for (const socket of link.sockets) socket.destroy(); link.sockets.clear(); };
  return link;
}
