// 让 cloudflare:test 的 env（Cloudflare.Env）获得与 wrangler.toml 绑定一致的类型。
import type { Env as WorkerEnv } from "../src/index";

declare global {
  namespace Cloudflare {
    interface Env extends WorkerEnv {}
  }
}
