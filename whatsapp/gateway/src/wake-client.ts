import type { CityWake } from "./wake.js";
import type { Deps } from "./gateway.js";

export interface Wake {
  enabled?: boolean;
  open(city: string, credentialHash: string): Promise<Response>;
  notify(city: string, liveHashes: string[]): Promise<void>;
}

export function createWake(namespace?: DurableObjectNamespace<CityWake>, mode?: string): Wake | undefined {
  if (!namespace) return undefined;
  const object = (city: string) => namespace.get(namespace.idFromName(city));
  return {
    enabled: mode !== "off",
    open: (city, hash) => object(city).fetch(new Request("https://wake.internal/", {
      headers: { upgrade: "websocket", "x-wake-credential": hash },
    })),
    notify: (city, liveHashes) => mode === "off" ? object(city).closeAll() : object(city).wake(liveHashes),
  };
}

/** A post-commit hint: failures never escape to the webhook or change its stored rows. */
export async function wakeDeliveries(deps: Deps, messageIds: string[]): Promise<void> {
  if (!deps.wake || !messageIds.length) return;
  await dispatchHints(deps, async () => {
    // Read the committed routing result, including historical links and earlier successful batches.
    const rows = await deps.db.prepare(
      `SELECT DISTINCT d.city_id, c.token_sha256
       FROM deliveries d LEFT JOIN credentials c ON c.city_id = d.city_id AND c.revoked_at IS NULL
       WHERE d.body IS NOT NULL AND d.wamid IN (SELECT value FROM json_each(?))`,
    ).bind(JSON.stringify(messageIds)).all<Target>();
    return rows.results;
  });
}

/** An idle city's safety read also returns it to ordinary polling during rollback. */
export async function closeCity(deps: Deps, city: string): Promise<void> {
  if (deps.wake?.enabled !== false) return;
  await dispatchHints(deps, async () => [{ city_id: city, token_sha256: null }]);
}

type Target = { city_id: string; token_sha256: string | null };

async function dispatchHints(deps: Deps, targets: () => Promise<Target[]>): Promise<void> {
  const deadline = performance.now() + 2000;
  let expired = false;
  let outstanding = 0;
  let failed = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const dispatch = async () => {
      const rows = await targets();
      if (expired) return;
      const cities = new Map<string, string[]>();
      for (const row of rows) {
        if (!cities.has(row.city_id)) cities.set(row.city_id, []);
        if (row.token_sha256) cities.get(row.city_id)!.push(row.token_sha256);
      }
      outstanding = cities.size;
      await Promise.all([...cities].map(async ([city, hashes]) => {
        try { await deps.wake!.notify(city, hashes); }
        catch { if (!expired) failed += 1; }
        finally { if (!expired) outstanding -= 1; }
      }));
    };
    await Promise.race([
      dispatch(),
      new Promise<never>((_resolve, reject) => {
        // Reserve the last 100 ms of the total budget for best-effort failure accounting.
        timer = setTimeout(() => { expired = true; reject(new Error("wake_deadline")); }, 1900);
      }),
    ]);
  } catch {
    failed += Math.max(1, outstanding);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
  if (failed) {
    deps.log({ event: "wake_failed", count: failed });
    try {
      await Promise.race([
        deps.db.prepare("INSERT INTO stats (name, value) VALUES ('wake_failed', ?) ON CONFLICT (name) DO UPDATE SET value = value + excluded.value")
          .bind(failed).run(),
        new Promise<void>((resolve) => { timer = setTimeout(resolve, Math.max(0, deadline - performance.now())); }),
      ]);
    } catch { /* The delivery already committed; accounting must not change its answer. */ }
    finally { if (timer !== undefined) clearTimeout(timer); }
  }
}
