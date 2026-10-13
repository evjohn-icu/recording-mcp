// 每个测试文件启动时（isolatedStorage 为每文件全新存储）应用 D1 迁移。
import { env } from "cloudflare:test";
import migrationSql from "../migrations/0001_init.sql?raw";

await env.DB.exec(migrationSql);
