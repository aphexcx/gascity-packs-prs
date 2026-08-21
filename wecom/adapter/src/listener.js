// listener.js — the internal HTTP surface (/publish, /publish-media,
// /healthz), extracted from index.js so its request hygiene is testable
// (codex jg-d0xr finding 10). index.js owns the server lifecycle (UDS vs
// TCP bind, signals); this module owns what a request is allowed to be:
//
//   - EXACT pathname routing. The old startsWith matching routed
//     /publish<anything>; now only the three known paths answer (query
//     strings are tolerated, path suffixes are not).
//   - A body byte cap, enforced twice: Content-Length is refused up
//     front when it declares more than the cap, and the streamed bytes
//     are counted regardless — the header is a claim, not a bound. Both
//     answer 413 and then sever the connection: a caller that streams an
//     unbounded body must not hold the socket, and the poisoned
//     keep-alive stream cannot be reused anyway.
//   - hardenServer sets socket-level header/request deadlines so a
//     dribbling client cannot pin a connection open indefinitely.
//
// The bodies on this surface are small JSON (publish text tops out far
// below 1MiB — the media FILE travels by path, never in the body), so the
// default cap is deliberately tight.

export const defaultMaxBodyBytes = 1024 * 1024; // 1MiB

export function createRequestListener({ publisher, log = () => {}, maxBodyBytes = defaultMaxBodyBytes }) {
  const refuse413 = (req, res) => {
    // Flush the 413 before severing: destroy() alone could race the
    // response bytes out of existence.
    res.once('finish', () => req.destroy());
    res.writeHead(413, { Connection: 'close' }).end('request body too large');
  };

  const handle = (handler, label) => (req, res) => {
    const declared = Number(req.headers['content-length']);
    if (Number.isFinite(declared) && declared > maxBodyBytes) {
      log(`${label}: Content-Length ${declared} exceeds the ${maxBodyBytes}-byte body cap; refused`);
      refuse413(req, res);
      return;
    }
    // Stream-decode as UTF-8: coercing each Buffer chunk to a string
    // independently corrupts a multibyte sequence (Chinese, emoji) that
    // a chunk boundary happens to split.
    req.setEncoding('utf8');
    let body = '';
    let bytes = 0;
    let refused = false;
    req.on('data', (d) => {
      if (refused) return;
      bytes += Buffer.byteLength(d);
      if (bytes > maxBodyBytes) {
        refused = true;
        log(`${label}: request body exceeded the ${maxBodyBytes}-byte cap; aborted`);
        refuse413(req, res);
        return;
      }
      body += d;
    });
    req.on('error', () => { /* severed mid-body; the 413 (if any) is already out */ });
    req.on('end', () => {
      if (refused) return;
      handler(req, res, body).catch((err) => {
        log(`${label} handler error: ${err.message}`);
        if (!res.headersSent) res.writeHead(500).end();
      });
    });
  };

  const publish = handle(publisher.handlePublish, 'publish');
  const publishMedia = handle(publisher.handlePublishMedia, 'publish-media');

  return (req, res) => {
    let pathname;
    try {
      pathname = new URL(req.url, 'http://internal').pathname;
    } catch {
      res.writeHead(404).end();
      return;
    }
    if (req.method === 'GET' && pathname === '/healthz') {
      res.writeHead(200).end('ok');
      return;
    }
    if (req.method === 'POST' && pathname === '/publish-media') {
      publishMedia(req, res);
      return;
    }
    if (req.method === 'POST' && pathname === '/publish') {
      publish(req, res);
      return;
    }
    res.writeHead(404).end();
  };
}

// hardenServer bounds how long a connection may take to become a request:
// headers within 10s, the whole request within 60s. Publish bodies are
// tiny — anything slower is a stuck or hostile peer, and the adapter's
// own long-running work (chunked upload, gc round-trip) happens AFTER the
// body is read, inside the handler, unaffected by requestTimeout... which
// only covers receiving the request. Keep-alive idle stays at Node's
// default (5s), fine for gc's connection reuse.
export function hardenServer(server) {
  server.headersTimeout = 10000;
  server.requestTimeout = 60000;
  return server;
}
