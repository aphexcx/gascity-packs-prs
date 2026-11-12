import { DurableObject } from "cloudflare:workers";

/** Only the Worker binding can reach this object. Attachments contain a credential hash only. */
export class CityWake extends DurableObject<unknown> {
  constructor(ctx: DurableObjectState, env: unknown) {
    super(ctx, env);
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
  }

  async fetch(request: Request): Promise<Response> {
    const hash = request.headers.get("x-wake-credential");
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket" || !hash || !/^[a-f0-9]{64}$/.test(hash)) {
      return new Response(null, { status: 400 });
    }
    // workerd enumerates newest first, including after hibernation (covered by the local test).
    const open = this.ctx.getWebSockets().filter((socket) => socket.readyState === WebSocket.OPEN);
    if (open.length >= 4) open.at(-1)!.close(1008, "connection_limit");
    const pair = new WebSocketPair();
    pair[1].serializeAttachment(hash);
    this.ctx.acceptWebSocket(pair[1]);
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  wake(liveHashes: string[]): void {
    const live = new Set(liveHashes);
    for (const socket of this.ctx.getWebSockets()) {
      if (!live.has(socket.deserializeAttachment())) socket.close(4401, "unauthorized");
      else if (socket.readyState === WebSocket.OPEN) socket.send("wake");
    }
  }

  closeAll(): void {
    for (const socket of this.ctx.getWebSockets()) socket.close(4503, "disabled");
  }

  webSocketMessage(socket: WebSocket, message: string | ArrayBuffer): void {
    if (message === "ping") socket.send("pong");
    else socket.close(1008, "unsupported_frame");
  }

  webSocketClose(socket: WebSocket, code: number): void {
    socket.close(code === 1005 || code === 1006 ? 1000 : code);
  }
}
