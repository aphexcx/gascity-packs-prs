import { cloudflareTest, readD1Migrations } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// Tests run inside workerd against a real D1 (miniflare's local SQLite), with the migrations of
// record applied by test/setup.ts. Meta's Graph API is a fake injected per test (test/harness.ts);
// nothing here reaches the network. The secrets below are test values for the entry-point tests
// that go through SELF; every other test builds the gateway with its own config. MIGRATION_DB is a
// second, empty D1 that only the migration test touches: it applies 0001, writes v1 rows, then
// applies the rest, the order a deployed gateway sees them in. BACKFILL_DB is a third, for the
// 0003 backfill test (gp-w376), which does the same: storage is not reset between tests, so each
// test that starts from schema v1 has its own D1.
export default defineConfig(async () => {
  const migrations = await readD1Migrations("migrations");
  return {
    plugins: [
      cloudflareTest({
        wrangler: { configPath: "./wrangler.toml" },
        miniflare: {
          d1Databases: ["MIGRATION_DB", "BACKFILL_DB"],
          bindings: {
            TEST_MIGRATIONS: migrations,
            META_ACCESS_TOKEN: "test-access-token-entry",
            META_APP_SECRET: "test-app-secret-entry",
            META_VERIFY_TOKEN: "test-verify-token-entry",
            META_PHONE_NUMBER_ID: "106540352242922",
            ADMIN_TOKEN: "test-admin-token-entry",
          },
        },
      }),
    ],
    // The replay tables now exercise a real object call after every committed payload.
    test: { setupFiles: ["./test/setup.ts"], testTimeout: 120000 },
  };
});
