import { cloudflareTest } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

// 测试跑在 workerd 内（vitest-pool-workers 插件），绑定来自 wrangler.toml：
// DB (D1)、AUDIO (R2)。机密与开发开关通过 miniflare.bindings 注入（覆盖 [vars]）。
export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.toml" },
      miniflare: {
        bindings: {
          ADMIN_TOKEN: "test-admin-token",
          PAIRING_PEPPER: "test-pepper",
          ENABLE_DEBUG_FAULTS: "1",
          EVENT_RETENTION_DAYS: "30",
        },
      },
    }),
  ],
  test: {
    setupFiles: ["./test/apply-migrations.ts"],
    testTimeout: 30_000,
    hookTimeout: 30_000,
  },
});
