import { cloudflareTest, readD1Migrations } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// Tests run inside workerd against a real D1 (miniflare's local SQLite), with the migrations of
// record applied by test/setup.ts. Meta's Graph API is a fake injected per test (test/harness.ts);
// nothing here reaches the network. The secrets below are test values for the entry-point tests
// that go through SELF; every other test builds the gateway with its own config.
export default defineConfig(async () => {
  const migrations = await readD1Migrations("migrations");
  return {
    plugins: [
      cloudflareTest({
        wrangler: { configPath: "./wrangler.toml" },
        miniflare: {
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
    test: { setupFiles: ["./test/setup.ts"] },
  };
});
