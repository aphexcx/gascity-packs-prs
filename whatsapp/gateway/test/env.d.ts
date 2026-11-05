// The bindings the tests see: the Worker's own (wrangler.toml and the test secrets in
// vitest.config.ts) plus the migrations test/setup.ts applies.
declare namespace Cloudflare {
  interface Env {
    DB: D1Database;
    MIGRATION_DB: D1Database;
    BACKFILL_DB: D1Database;
    TEST_MIGRATIONS: import("cloudflare:test").D1Migration[];
    PUBLIC_URL: string;
    DISPLAY_NAME: string;
    GRAPH_VERSION: string;
    META_ACCESS_TOKEN: string;
    META_APP_SECRET: string;
    META_VERIFY_TOKEN: string;
    META_PHONE_NUMBER_ID: string;
    ADMIN_TOKEN: string;
  }
}
