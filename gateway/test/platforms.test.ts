// Platform adapters inside the built gateway, against the real Telegram Bot API and QQ open platform.
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import Database from "better-sqlite3";
import { afterEach, beforeAll, describe, expect, test } from "vitest";
import { openDb } from "../src/db/index.ts";
import { Repo } from "../src/repo.ts";
import {
  GatewayProcess,
  connectBridge,
  connectDaemon,
  makeEnv,
  removeEnv,
  requireBuild,
  sleep,
  type Env,
  type RpcPeer,
} from "./helpers.ts";

beforeAll(() => requireBuild());

const cleanups: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  while (cleanups.length > 0) await cleanups.pop()!();
});

async function start(env: Env): Promise<GatewayProcess> {
  const gw = new GatewayProcess(env);
  cleanups.push(() => gw.kill());
  await gw.ready();
  return gw;
}

async function waitPlatform(ctl: RpcPeer, name: string, accept: (p: any) => boolean, timeoutMs: number): Promise<any> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const { platforms } = await ctl.request("platforms.status", {});
    const p = platforms.find((x: any) => x.name === name);
    if (p && accept(p)) return p;
    if (Date.now() > deadline) throw new Error(`platform ${name} status: ${JSON.stringify(p)}`);
    await sleep(250);
  }
}

describe("a failed first connection does not stop the gateway (contracts 9.4)", () => {
  test("telegram with an invalid token: recorded as error and retried, console keeps working", async () => {
    const env = makeEnv({ platforms: { telegram: { botToken: "123456:invalid-token-for-bearbot-tests" } } });
    cleanups.push(() => removeEnv(env));
    const gw = await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const tg = await waitPlatform(ctl, "telegram", (p) => p.state === "error", 30_000);
    expect(tg.lastError).toEqual(expect.any(String));
    expect(tg.lastErrorAt).toEqual(expect.any(String));
    await gw.waitLog(/"platform":"telegram".*"msg":"platform connection failed"/);

    const { peer, hello } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    expect(Object.keys(hello.self).sort()).toEqual(["console", "telegram"]);
    gw.type("console still works");
    await peer.waitMessage((m) => m.text === "console still works");

    // A second attempt happens after the 1 s backoff.
    await gw.waitLog(/"retryInMs":2000/, 20_000);
    gw.child.kill("SIGTERM");
    await gw.exited;
    expect(gw.exitCode).toBe(0);
  });

  test("qq with made-up credentials: recorded as error and retried", async () => {
    const env = makeEnv({ platforms: { qq: { appId: "102000000", clientSecret: "not-a-real-secret", sandbox: true } } });
    cleanups.push(() => removeEnv(env));
    const gw = await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const qq = await waitPlatform(ctl, "qq", (p) => p.state === "error", 30_000);
    expect(qq.lastError).toEqual(expect.any(String));
    const { peer, hello } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    expect(hello.self.qq).toBe("bearbot");
    gw.type("ok");
    await peer.waitMessage((m) => m.text === "ok");
  });
});

const TG_TOKEN = process.env.BEARBOT_TEST_TELEGRAM_BOT_TOKEN;
const TG_CHAT = process.env.BEARBOT_TEST_TELEGRAM_CHAT_ID;

describe.skipIf(!TG_TOKEN || !TG_CHAT)("live Telegram (BEARBOT_TEST_TELEGRAM_BOT_TOKEN, BEARBOT_TEST_TELEGRAM_CHAT_ID)", () => {
  test("connects, splits long text, sends files, edits, reacts, deletes", async () => {
    const env = makeEnv({ platforms: { telegram: { botToken: TG_TOKEN } } });
    cleanups.push(() => removeEnv(env));
    const target = `telegram:${TG_CHAT}`;
    // Bind the test chat before the gateway starts (an admin would do this with /bb attach).
    const db = openDb(env.database);
    new Repo(db, { consoleEnabled: true, classifierEnabledDefault: true, platforms: new Set(["telegram", "console"]) }).putBinding({
      channelId: target,
      platform: "telegram",
      channelName: "bearbot test",
      trigger: "mention",
      prefix: null,
      replyMode: "final",
      interrupt: "queue",
      createdBy: "test",
    });
    db.$client.close();

    await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    await waitPlatform(ctl, "telegram", (p) => p.state === "connected", 60_000);
    const { peer, hello } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    expect(hello.bindings.map((b: any) => b.channel)).toContain(target);
    expect(hello.self.telegram).not.toBe("bearbot");

    const long = Array.from({ length: 600 }, (_, i) => `第 ${i} 行 *测试* a.b-c (分段)`).join("\n");
    const { ref } = await peer.request("post", { target, markdown: long });
    expect(ref.startsWith(`${target}|`)).toBe(true);
    const history = await peer.request("history", { target, limit: 50 });
    const out = history.messages.filter((m: any) => m.direction === "out");
    expect(out.length).toBeGreaterThan(1);
    expect(out.map((m: any) => m.text).join("\n")).toBe(long);

    writeFileSync(join(env.workspace, "note.txt"), "bearbot gateway live test");
    await peer.request("post", { target, markdown: "附件测试", files: [{ path: "note.txt" }] });
    await peer.request("typing", { target });

    const { ref: small } = await peer.request("post", { target, markdown: "将被编辑" });
    await peer.request("edit", { ref: small, markdown: "已编辑 **完成**" });
    await peer.request("react", { ref: small, emoji: "thumbs_up" });
    await peer.request("react", { ref: small, emoji: "thumbs_up", remove: true });
    await peer.request("delete", { ref: small });
  });
});

const QQ_APP = process.env.BEARBOT_TEST_QQ_APP_ID;
const QQ_SECRET = process.env.BEARBOT_TEST_QQ_CLIENT_SECRET;

describe.skipIf(!QQ_APP || !QQ_SECRET)("live QQ sandbox (BEARBOT_TEST_QQ_APP_ID, BEARBOT_TEST_QQ_CLIENT_SECRET)", () => {
  test("socket mode connects and platforms.status reports it", async () => {
    const env = makeEnv({ platforms: { qq: { appId: QQ_APP, clientSecret: QQ_SECRET, sandbox: true } } });
    cleanups.push(() => removeEnv(env));
    const gw = await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const qq = await waitPlatform(ctl, "qq", (p) => p.state === "connected", 60_000);
    expect(qq.since).toEqual(expect.any(String));
    expect(await ctl.request("drain", {})).toEqual({});
    gw.child.kill("SIGTERM");
    await gw.exited;
    expect(gw.exitCode).toBe(0);
  });

  const QQ_GROUP = process.env.BEARBOT_TEST_QQ_GROUP_OPENID;

  test.skipIf(!QQ_GROUP)("a message of the bot gets a quote index and can be quoted (BEARBOT_TEST_QQ_GROUP_OPENID)", async () => {
    const env = makeEnv({ platforms: { qq: { appId: QQ_APP, clientSecret: QQ_SECRET, sandbox: true } } });
    cleanups.push(() => removeEnv(env));
    const target = `qq:group/${QQ_GROUP}`;
    // Bind the test group before the gateway starts (an admin would do this with /bb attach).
    const db = openDb(env.database);
    new Repo(db, { consoleEnabled: true, classifierEnabledDefault: true, platforms: new Set(["qq", "console"]) }).putBinding({
      channelId: target,
      platform: "qq",
      channelName: "bearbot test",
      trigger: "mention",
      prefix: null,
      replyMode: "final",
      interrupt: "queue",
      createdBy: "test",
    });
    db.$client.close();

    await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    await waitPlatform(ctl, "qq", (p) => p.state === "connected", 60_000);
    const { peer } = await connectBridge(env);
    cleanups.push(() => void peer.close());

    const { ref } = await peer.request("post", { target, markdown: "bearbot 引用测试：被引用的消息" });
    const messageId = ref.slice(ref.indexOf("|") + 1);
    const quoteIdOf = (id: string): string | null => {
      const reader = new Database(env.database, { readonly: true });
      try {
        return (reader.prepare("select quote_id from message_log where message_id = ?").get(id) as { quote_id: string | null }).quote_id;
      } finally {
        reader.close();
      }
    };
    expect(quoteIdOf(messageId)).toMatch(/^REFIDX_/);

    const { ref: reply } = await peer.request("post", { target, markdown: "bearbot 引用测试：引用上一条", replyTo: messageId });
    expect(reply.startsWith(`${target}|`)).toBe(true);
    const missing = await peer.requestError("post", { target, markdown: "不会发出", replyTo: "ROBOT1.0_missing" });
    expect(missing).toMatchObject({ code: -32000, message: expect.stringContaining("找不到要回复的消息：ROBOT1.0_missing") });
  });
});
