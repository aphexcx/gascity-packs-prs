/**
 * The Worker entry for whatsapp.dynasty.computer: binds the environment to the gateway
 * (src/gateway.ts) and runs the daily maintenance on the cron. Logs carry counts and fixed event
 * names only: never a body, a phone number, a token or a secret.
 */
import { createGateway } from "./gateway.js";

export interface Env {
  DB: D1Database;
  META_ACCESS_TOKEN: string;
  META_APP_SECRET: string;
  META_VERIFY_TOKEN: string;
  META_PHONE_NUMBER_ID: string;
  ADMIN_TOKEN: string;
  GRAPH_VERSION: string;
  DISPLAY_NAME: string;
  PUBLIC_URL: string;
}

function gateway(env: Env) {
  return createGateway({
    db: env.DB,
    config: {
      metaAccessToken: env.META_ACCESS_TOKEN ?? "",
      metaAppSecret: env.META_APP_SECRET ?? "",
      metaVerifyToken: env.META_VERIFY_TOKEN ?? "",
      metaPhoneNumberId: env.META_PHONE_NUMBER_ID ?? "",
      adminToken: env.ADMIN_TOKEN ?? "",
      graphVersion: env.GRAPH_VERSION,
      displayName: env.DISPLAY_NAME,
    },
    now: () => Math.floor(Date.now() / 1000),
    fetch: (input, init) => fetch(input, init),
    log: (event) => console.log(JSON.stringify(event)),
  });
}

export default {
  fetch(request: Request, env: Env): Promise<Response> {
    return gateway(env).handle(request);
  },
  async scheduled(_controller: ScheduledController, env: Env): Promise<void> {
    const report = await gateway(env).maintain();
    console.log(JSON.stringify({ event: "maintenance", ...report }));
  },
} satisfies ExportedHandler<Env>;
