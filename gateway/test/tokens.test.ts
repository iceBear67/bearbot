// DESIGN 6.6 token flow on a real gateway database. IM platforms are needed to send a token as a chat
// user end to end; here the claim path of tokens.ts runs against the same tables the gateway uses.
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test } from "vitest";
import { openDb, type Db } from "../src/db/index.ts";
import { Repo } from "../src/repo.ts";
import { TOKEN_LIKE, Tokens } from "../src/tokens.ts";

let dir: string;
let db: Db;
let tokens: Tokens;
let repo: Repo;

beforeEach(() => {
  dir = mkdtempSync("/tmp/bbtok");
  db = openDb(join(dir, "gateway.db"));
  tokens = new Tokens(db);
  repo = new Repo(db, { consoleEnabled: true, classifierEnabledDefault: true, platforms: new Set(["telegram", "console"]) });
});

afterEach(() => {
  db.$client.close();
  rmSync(dir, { recursive: true, force: true });
});

test("token format: bearbot- and 26 base32 characters, only the SHA-256 hash is stored", () => {
  const { token, expiresAt } = tokens.create(600_000);
  expect(token).toMatch(/^bearbot-[a-z2-7]{26}$/);
  expect(TOKEN_LIKE.test(token)).toBe(true);
  expect(Math.abs(expiresAt.getTime() - (Date.now() + 600_000))).toBeLessThan(2000);
  const rows = db.$client.prepare("select * from admin_token").all() as { token_hash: string }[];
  expect(rows).toHaveLength(1);
  expect(rows[0]!.token_hash).toMatch(/^[0-9a-f]{64}$/);
  expect(JSON.stringify(rows)).not.toContain(token.slice("bearbot-".length));
});

test("claim makes the sender an admin with notify_thread = the thread, once", () => {
  const { token } = tokens.create(600_000);
  expect(tokens.claim(token, { identity: "telegram:42", displayName: "Alice", thread: "telegram:42" })).toBe("claimed");
  expect(repo.isAdmin("telegram:42")).toBe(true);
  const admin = repo.adminRow("telegram:42")!;
  expect(admin.notifyThread).toBe("telegram:42");
  expect(admin.displayName).toBe("Alice");
  expect(admin.addedBy).toBe("token");
  // single use
  expect(tokens.claim(token, { identity: "telegram:43", displayName: "Bob", thread: "telegram:-100" })).toBe("invalid");
  expect(repo.isAdmin("telegram:43")).toBe(false);
  const used = db.$client.prepare("select used_by, used_at from admin_token").get() as { used_by: string; used_at: number };
  expect(used.used_by).toBe("telegram:42");
  expect(used.used_at).toBeGreaterThan(0);
});

test("tokens are matched case-insensitively", () => {
  const { token } = tokens.create(600_000);
  expect(tokens.claim(token.toUpperCase(), { identity: "qq:abc", displayName: null, thread: "qq:c2c/abc" })).toBe("claimed");
});

test("an expired token is refused", async () => {
  const { token } = tokens.create(1);
  await new Promise((r) => setTimeout(r, 10));
  expect(tokens.claim(token, { identity: "telegram:42", displayName: null, thread: "telegram:42" })).toBe("invalid");
  expect(repo.isAdmin("telegram:42")).toBe(false);
});

test("an unknown token is refused", () => {
  expect(tokens.claim("bearbot-aaaaaaaaaaaaaaaaaaaaaaaaaa", { identity: "telegram:1", displayName: null, thread: "telegram:1" })).toBe(
    "invalid",
  );
});

test("claiming again with a new token updates the notify thread of an existing admin", () => {
  tokens.claim(tokens.create(600_000).token, { identity: "telegram:42", displayName: "A", thread: "telegram:42" });
  tokens.claim(tokens.create(600_000).token, { identity: "telegram:42", displayName: "A2", thread: "telegram:-100:7" });
  expect(repo.adminRow("telegram:42")!.notifyThread).toBe("telegram:-100:7");
  expect(repo.adminIdentities()).toEqual(["telegram:42", "console:operator"]);
});
