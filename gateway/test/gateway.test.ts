// The built gateway (node dist/main.js --config <tmp>) with the console in line mode on pipes (fds 3 and 4).
// The test connects to gateway-ctl.sock like bearbotd and to gateway.sock like bearbot-im.
import { execFileSync, spawn } from "node:child_process";
import {
  existsSync,
  lstatSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  symlinkSync,
  truncateSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { connect } from "node:net";
import { join } from "node:path";
import type { Readable } from "node:stream";
import { afterEach, beforeAll, describe, expect, test } from "vitest";
import { openDb } from "../src/db/index.ts";
import { Repo } from "../src/repo.ts";
import {
  CLOCK,
  GatewayProcess,
  RpcPeer,
  connectBridge,
  connectDaemon,
  gatewayDate,
  makeEnv,
  removeEnv,
  requireBuild,
  sleep,
  waitFor,
  writeConfig,
  type Env,
} from "./helpers.ts";

const ISO_LOCAL = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2}$/;
const ID = /^\d+-\d+$/;
/** A 1x1 PNG. */
const PNG = Buffer.from(
  "89504e470d0a1a0a0000000d4948445200000001000000010806000000" + "1f15c4890000000d49444154789c6360000002000154a24f5d0000000049454e44ae426082",
  "hex",
);

beforeAll(() => requireBuild());

const cleanups: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  while (cleanups.length > 0) {
    await cleanups.pop()!();
  }
});

async function start(overrides: Record<string, unknown> = {}, env?: Env): Promise<{ env: Env; gw: GatewayProcess }> {
  const e = env ?? makeEnv(overrides);
  if (!env) cleanups.push(() => removeEnv(e));
  const gw = new GatewayProcess(e);
  cleanups.push(() => gw.kill());
  await gw.ready();
  return { env: e, gw };
}

function track<T extends { close(): Promise<void> }>(peer: T): T {
  cleanups.push(() => void peer.close());
  return peer;
}

function nthOf(gw: GatewayProcess): number {
  return gw.consoleOut.length;
}

describe("hello, agent.state and console messages", () => {
  test("a console message reaches bearbot-im with the WireMessage fields, ack, post/edit/react/delete output", async () => {
    const { env, gw } = await start();
    await gw.waitConsole(new RegExp(`^\\[${CLOCK} 状态\\] 控制台已连接\\n`));
    const ctl = track(await connectDaemon(env));
    await gw.waitConsole(new RegExp(`\\[${CLOCK} 状态\\] agent：running\\n`));

    const { peer, hello } = await connectBridge(env);
    track(peer);
    expect(hello).toEqual({
      bindings: [
        { channel: "console:main", name: "控制台", platform: "console", trigger: "all", prefix: null, replyMode: "final", interrupt: "queue" },
      ],
      admins: ["console:operator"],
      classifier: { enabled: true, rules: [] },
      self: { console: "bearbot" },
    });

    gw.type("hello world");
    const m = await peer.waitMessage((x) => x.text === "hello world");
    expect(m).toMatchObject({
      seq: [1],
      kind: "message",
      platform: "console",
      channel: { id: "console:main", name: "控制台", isDM: true },
      thread: "console:main",
      sender: { id: "console:operator", name: "控制台", role: "admin", isBot: false },
      mentionsBot: true,
      edited: false,
      stale: false,
      attachments: [],
      trigger: true,
    });
    expect(m.id).toMatch(ID);
    expect(m.time).toMatch(ISO_LOCAL);
    expect(Object.keys(m).sort()).toEqual(
      ["attachments", "channel", "edited", "id", "kind", "mentionsBot", "platform", "sender", "seq", "stale", "text", "thread", "time", "trigger"],
    );
    expect(await peer.request("ack", { seq: 1 })).toEqual({});

    const from = nthOf(gw);
    const { ref } = await peer.request("post", { target: "console:main", markdown: "**你好**\n第二行" });
    expect(ref).toMatch(/^console:main\|\d+-\d+$/);
    const n = ref.split("-").at(-1);
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #${n}\\]\\n\\*\\*你好\\*\\*\\n第二行\\n`), from);

    await peer.request("edit", { ref, markdown: "改过的内容" });
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #${n} 已编辑\\]\\n改过的内容\\n`), from);
    await peer.request("react", { ref, emoji: "thumbs_up" });
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #${n} \\+👍\\]\\n`), from);
    await peer.request("react", { ref, emoji: "👍", remove: true });
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #${n} -👍\\]\\n`), from);
    await peer.request("delete", { ref });
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #${n} 已删除\\]\\n`), from);
    expect(await peer.request("typing", { target: "console:main" })).toEqual({});

    const card = { type: "card", title: "确认", children: [{ type: "text", content: "订单 42" }] };
    const cardRef = await peer.request("post", { target: "console:main", card });
    const cn = cardRef.ref.split("-").at(-1);
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #${cn}\\]\\n  \\{\\n    "type": "card",\\n    "title": "确认",`), from);

    const history = await peer.request("history", { target: "console:main", limit: 10 });
    expect(history.messages.map((x: any) => [x.direction, x.text])).toEqual([
      ["in", "hello world"],
      ["out", "**你好**\n第二行"],
      ["out", ""],
    ]);
    expect(history.messages[1]).toMatchObject({
      kind: "message",
      platform: "console",
      thread: "console:main",
      sender: { id: "console:bot", name: "bearbot", role: "member", isBot: true },
      trigger: false,
      direction: "out",
    });
    const older = await peer.request("history", { target: "console:main", limit: 10, before: String(history.messages[1].seq[0]) });
    expect(older.messages.map((x: any) => x.text)).toEqual(["hello world"]);

    const targets = await peer.request("targets", {});
    expect(targets.targets).toHaveLength(1);
    expect(targets.targets[0]).toMatchObject({ channel: "console:main", name: "控制台", platform: "console", isDM: true });
    expect(targets.targets[0].threads[0].thread).toBe("console:main");
    expect(targets.targets[0].threads[0].lastActiveAt).toMatch(ISO_LOCAL);

    expect(await peer.request("origin", { requesters: null, thread: null })).toEqual({ target: "console:main" });
    expect(await peer.request("origin", { requesters: ["console:operator"], thread: "telegram:1" })).toEqual({ target: "console:main" });
    expect(await peer.request("origin", { requesters: ["telegram:9"], thread: null })).toEqual({ target: null });
    expect(await peer.request("origin", { requesters: null, thread: "console:main" })).toEqual({ target: "console:main" });

    expect(await ctl.request("platforms.status", {})).toMatchObject({ platforms: [{ name: "console", state: "line" }] });
    gw.endInput();
    await waitFor("console input-ended", () => (/console input ended/.test(gw.logOut) ? true : undefined));
    expect(await ctl.request("platforms.status", {})).toMatchObject({ platforms: [{ name: "console", state: "input-ended" }] });
    // Output keeps working after the input ended.
    await peer.request("post", { target: "console:main", markdown: "输入结束后" });
    await gw.waitConsole(/输入结束后\n/, from);
  });

  test("local commands, :: escape and backslash continuation", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    gw.type("::abc");
    expect((await peer.waitMessage((x) => x.text === ":abc")).trigger).toBe(true);
    gw.type("第一行\\");
    gw.type("第二行\\");
    gw.type("第三行");
    await peer.waitMessage((x) => x.text === "第一行\n第二行\n第三行");
    gw.type(":nosuch");
    await gw.waitConsole(new RegExp(`\\[${CLOCK} 错误\\] 未知的本地命令：:nosuch`));
    gw.type(":help");
    await gw.waitConsole(/本地命令：\n  :help/);
    gw.type("   ");
    gw.type("");
    gw.type("last");
    await peer.waitMessage((x) => x.text === "last");
    expect(peer.messages().map((x) => x.text)).toEqual([":abc", "第一行\n第二行\n第三行", "last"]);
  });

  test(":file sends a workspace file as an attachment saved in inbox/<date>/, images carry base64 data", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    mkdirSync(join(env.workspace, "docs"));
    writeFileSync(join(env.workspace, "docs", "a.txt"), "附件内容");
    const png = PNG;
    writeFileSync(join(env.workspace, "p.png"), png);

    gw.type(":file docs/a.txt 看看这个文件");
    const m = await peer.waitMessage((x) => x.text === "看看这个文件");
    expect(m.attachments).toHaveLength(1);
    const a = m.attachments[0];
    expect(a).toMatchObject({ type: "file", name: "a.txt", mimeType: "text/plain", size: Buffer.byteLength("附件内容") });
    expect(a.path).toMatch(new RegExp(`^inbox/\\d{4}-\\d{2}-\\d{2}/${m.id}-a\\.txt$`));
    expect(readFileSync(join(env.workspace, a.path), "utf8")).toBe("附件内容");
    expect(a.data).toBeUndefined();

    gw.type(`:file ${join(env.workspace, "p.png")}`);
    const img = await peer.waitMessage((x) => x.attachments.length === 1 && x.attachments[0].name === "p.png");
    expect(img.text).toBe("");
    expect(img.attachments[0]).toMatchObject({ type: "image", mimeType: "image/png", size: png.length });
    expect(Buffer.from(img.attachments[0].data, "base64").equals(png)).toBe(true);

    gw.type(":file missing.txt");
    await gw.waitConsole(new RegExp(`\\[${CLOCK} 错误\\] 无法读取文件 .*missing\\.txt`));

    // Strict seq order: a message after one with a running download waits for it.
    writeFileSync(join(env.workspace, "big.bin"), Buffer.alloc(32 * 1024 * 1024, 1));
    gw.type(":file big.bin 大文件");
    gw.type("在大文件之后");
    await peer.waitMessage((x) => x.text === "在大文件之后");
    const texts = peer.messages().map((x) => x.text);
    expect(texts.slice(-2)).toEqual(["大文件", "在大文件之后"]);
    const big = peer.messages().find((x) => x.text === "大文件");
    expect(big.attachments[0]).toMatchObject({ type: "file", name: "big.bin", size: 32 * 1024 * 1024 });
    expect(big.seq[0]).toBeLessThan(peer.messages().at(-1).seq[0]);
  });

  test("a download slower than attachmentTimeoutMs is sent with an error", async () => {
    const { env, gw } = await start({ attachmentTimeoutMs: 1 });
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    writeFileSync(join(env.workspace, "huge.bin"), Buffer.alloc(64 * 1024 * 1024, 2));
    gw.type(":file huge.bin 超时");
    const m = await peer.waitMessage((x) => x.text === "超时");
    expect(m.attachments).toEqual([
      { type: "file", name: "huge.bin", size: 64 * 1024 * 1024, mimeType: "application/octet-stream", error: "下载超时（1 毫秒）" },
    ]);
  });

  test("other slash commands become kind=command messages", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    gw.type("  /weather 上海  明天");
    const m = await peer.waitMessage((x) => x.kind === "command");
    expect(m).toMatchObject({ command: { name: "/weather", args: "上海  明天" }, text: "/weather 上海  明天", trigger: true, mentionsBot: true });
    expect(m.id).toMatch(ID);
  });
});

describe("delivery rule", () => {
  test("messages wait until the agent is running and bearbot-im connected; replay starts after the acked cursor", async () => {
    const { env, gw } = await start();
    const ctl = track(await connectDaemon(env, "starting"));
    gw.type("m1");
    gw.type("m2");
    const { peer } = await connectBridge(env);
    track(peer);
    await sleep(300);
    expect(peer.messages()).toHaveLength(0);
    await ctl.request("agent.state", { state: "running", reason: null });
    await peer.waitMessage((x) => x.text === "m2");
    expect(peer.messages().map((x) => x.seq[0])).toEqual([1, 2]);
    await peer.request("ack", { seq: 1 });
    gw.type("m3");
    await peer.waitMessage((x) => x.text === "m3");
    await peer.close();

    // Reconnect: everything after the acked seq 1 comes again, in order.
    const second = await connectBridge(env);
    track(second.peer);
    await second.peer.waitMessage((x) => x.text === "m3");
    expect(second.peer.messages().map((x) => x.text)).toEqual(["m2", "m3"]);
    await second.peer.close();

    // ackedSeq from bearbot-im is honoured and stored.
    const third = await connectBridge(env, 3);
    track(third.peer);
    gw.type("m4");
    await third.peer.waitMessage((x) => x.text === "m4");
    expect(third.peer.messages().map((x) => x.text)).toEqual(["m4"]);
  });

  test("seqs bearbot-im completed above the acked cursor are not sent again", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    for (const text of ["m1", "m2", "m3"]) gw.type(text);
    await peer.waitMessage((x) => x.text === "m3");
    expect(peer.messages().map((x) => x.seq[0])).toEqual([1, 2, 3]);
    // seq 2 is still queued in bearbot-im while seqs 1 and 3 are complete.
    expect(await peer.request("ack", { seq: 1, done: [3] })).toEqual({});
    await peer.close();

    const second = await connectBridge(env);
    track(second.peer);
    gw.type("m4");
    await second.peer.waitMessage((x) => x.text === "m4");
    expect(second.peer.messages().map((x) => x.text)).toEqual(["m2", "m4"]);
    await second.peer.close();

    // hello carries the completed seqs as well.
    const third = await connectBridge(env, 1, [2]);
    track(third.peer);
    gw.type("m5");
    await third.peer.waitMessage((x) => x.text === "m5");
    expect(third.peer.messages().map((x) => x.text)).toEqual(["m4", "m5"]);
    await expect(third.peer.request("ack", { seq: 5, done: [0] })).rejects.toThrow("done must be an array of integers >= 1");
  });

  test("rows older than replayWindowMs are sent with trigger false and stale true", async () => {
    const { env, gw } = await start({ replayWindowMs: 500 });
    track(await connectDaemon(env));
    gw.type("old message");
    // chat-sdk logs this line in the same synchronous run that records the row, so the age counts from here.
    await gw.waitLog(/"Direct message received/);
    await sleep(800);
    const { peer } = await connectBridge(env);
    track(peer);
    const old = await peer.waitMessage((x) => x.text === "old message");
    expect(old).toMatchObject({ stale: true, trigger: false });
    gw.type("fresh");
    expect(await peer.waitMessage((x) => x.text === "fresh")).toMatchObject({ stale: false, trigger: true });
  });

  test("delivery survives a gateway restart (same database)", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    const first = await start({}, env);
    track(await connectDaemon(env, "repairing"));
    first.gw.type("before restart");
    await waitFor("row", () => (/"Direct message received/.test(first.gw.logOut) ? true : undefined));
    await sleep(200);
    await first.gw.stop();
    expect(first.gw.exitCode).toBe(0);

    const second = await start({}, env);
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    expect((await peer.waitMessage((x) => x.text === "before restart")).seq).toEqual([1]);
    second.gw.type("after restart");
    // seq 2 is the maintenance notice sent before the restart (outbound rows share the seq space).
    expect((await peer.waitMessage((x) => x.text === "after restart")).seq).toEqual([3]);
    expect(peer.messages().map((x) => x.text)).toEqual(["before restart", "after restart"]);
  });

  test("a message recorded before SIGKILL is delivered after the restart", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    const first = await start({}, env);
    track(await connectDaemon(env, "repairing"));
    first.gw.type("before kill");
    // The maintenance notice is posted after the row is recorded.
    await first.gw.waitConsole(/bearbot 正在修复，暂时无法回复。\n/);
    await first.gw.kill();
    expect(first.gw.exitSignal).toBe("SIGKILL");

    await start({}, env);
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    expect((await peer.waitMessage((x) => x.text === "before kill")).seq).toEqual([1]);
  });

  test("an attachment download cut off by SIGKILL is delivered with an error after the restart", async () => {
    const env = makeEnv({ attachmentTimeoutMs: 600_000 });
    cleanups.push(() => removeEnv(env));
    const first = await start({}, env);
    track(await connectDaemon(env, "repairing"));
    // A sparse file: reading and saving 256 MiB takes far longer than the kill below.
    const big = join(env.workspace, "big.bin");
    writeFileSync(big, "");
    truncateSync(big, 256 * 1024 * 1024);
    // Killed as soon as the maintenance notice (posted right after the row is recorded) appears.
    const consoleOut = first.gw.child.stdio[4] as Readable;
    consoleOut.on("data", (d: string) => {
      if (d.includes("bearbot 正在修复，暂时无法回复。")) {
        first.gw.child.kill("SIGKILL");
      }
    });
    first.gw.type(":file big.bin 大文件");
    await first.gw.exited;
    expect(first.gw.exitSignal).toBe("SIGKILL");

    const second = await start({}, env);
    await second.gw.waitLog(/"msg":"attachment download interrupted by a restart"/);
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    const m = await peer.waitMessage((x) => x.text === "大文件");
    expect(m.attachments).toEqual([
      { type: "file", name: "big.bin", size: 256 * 1024 * 1024, mimeType: "application/octet-stream", error: "gateway 重启，附件没有下载完成" },
    ]);
  });
});

describe("maintenance notice", () => {
  test("once per channel per maintenance period, also across a SIGKILL restart; messages are still recorded and replayed", async () => {
    const notice = "bearbot 正在修复，暂时无法回复。";
    const notices = (gw: GatewayProcess) => gw.consoleOut.split(notice).length - 1;
    const { env, gw } = await start();
    const ctl = track(await connectDaemon(env, "repairing"));
    gw.type("one");
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #\\d+\\]\\n${notice}\\n`));
    gw.type("two");
    await ctl.request("agent.state", { state: "maintenance", reason: "recovery 无法修复" });
    await gw.waitConsole(new RegExp(`agent：repairing → maintenance（recovery 无法修复）`));
    gw.type("three");
    await sleep(500);
    expect(notices(gw)).toBe(1);

    // DESIGN 5.5: bearbotd restarts a killed gateway and sends agent.state again; the period goes on.
    await gw.kill();
    const again = (await start({}, env)).gw;
    const ctl2 = track(await connectDaemon(env, "maintenance"));
    again.type("after kill");
    await again.waitLog(/"Direct message received/);
    await sleep(500);
    expect(notices(again)).toBe(0);

    await ctl2.request("agent.state", { state: "running", reason: null });
    const { peer } = await connectBridge(env);
    track(peer);
    await peer.waitMessage((x) => x.text === "after kill");
    expect(peer.messages().map((x) => x.text)).toEqual(["one", "two", "three", "after kill"]);

    // A new maintenance period: the channel gets the notice again, once.
    await ctl2.request("agent.state", { state: "rolling_back", reason: "管理员回滚" });
    again.type("four");
    await waitFor("notice in the new period", () => (notices(again) === 1 ? true : undefined));
    again.type("five");
    // "after kill", "four", "five" were received by this process.
    await waitFor("five received", () => (again.logOut.split('"Direct message received').length - 1 === 3 ? true : undefined));
    await sleep(500);
    expect(notices(again)).toBe(1);
  });
});

describe("gateway.sock rules", () => {
  test("a second hello is rejected while one connection is active; accepted after it closed", async () => {
    const { env } = await start();
    const first = await connectBridge(env);
    const other = track(await RpcPeer.connect(env.gatewaySocket));
    const err = await other.requestError("hello", {});
    expect(err.code).toBe(-32000);
    expect(err.message).toMatch(/已有 bearbot-im 连接/);
    await first.peer.close();
    await sleep(100);
    const ok = await other.request("hello", {});
    expect(ok.admins).toEqual(["console:operator"]);
  });

  test("requests before hello are refused", async () => {
    const { env } = await start();
    const peer = track(await RpcPeer.connect(env.gatewaySocket));
    expect((await peer.requestError("post", { target: "console:main", markdown: "x" })).message).toMatch(/hello/);
    expect((await peer.requestError("nosuch", {})).code).toBe(-32601);
  });

  test("target validation and file path checks", async () => {
    const { env } = await start();
    const { peer } = await connectBridge(env);
    track(peer);
    for (const target of ["telegram:123", "console:other", "qq:c2c/x", "nonsense"]) {
      const e = await peer.requestError("post", { target, markdown: "x" });
      expect(e.code).toBe(-32000);
      expect(e.message).toMatch(/不是已绑定的频道/);
    }
    expect((await peer.requestError("typing", { target: "telegram:1" })).message).toMatch(/不是已绑定的频道/);
    expect((await peer.requestError("history", { target: "telegram:1", limit: 5 })).message).toMatch(/不是已绑定的频道/);
    expect((await peer.requestError("edit", { ref: "telegram:1|5", markdown: "x" })).message).toMatch(/不是已绑定的频道/);
    expect((await peer.requestError("post", { target: "console:main" })).code).toBe(-32602);
    expect((await peer.requestError("history", { target: "console:main", limit: 0 })).code).toBe(-32602);
    expect((await peer.requestError("post", { target: "console:main", card: { title: "no type" } })).code).toBe(-32602);

    writeFileSync(join(env.root, "outside.txt"), "secret");
    symlinkSync(join(env.root, "outside.txt"), join(env.workspace, "link.txt"));
    for (const path of [join(env.root, "outside.txt"), "../outside.txt", "link.txt"]) {
      const e = await peer.requestError("post", { target: "console:main", markdown: "x", files: [{ path }] });
      expect(e.message).toMatch(/文件不在工作区目录内/);
    }
    expect((await peer.requestError("post", { target: "console:main", files: [{ path: "nope.txt" }] })).message).toMatch(/无法读取文件/);
  });

  test("files are saved under consoleFilesDir/<date>/<msgid>-<name> and listed in the output", async () => {
    const { env, gw } = await start();
    const { peer } = await connectBridge(env);
    track(peer);
    writeFileSync(join(env.workspace, "report.pdf"), Buffer.alloc(2048, 7));
    const from = nthOf(gw);
    const { ref } = await peer.request("post", {
      target: "console:main",
      markdown: "报告见附件",
      files: [{ path: "report.pdf" }, { path: join(env.workspace, "report.pdf"), name: "副本.pdf" }],
    });
    const id = ref.split("|")[1];
    const m = await gw.waitConsole(
      new RegExp(`报告见附件\\n  \\[文件\\] report\\.pdf，2048 字节，application/pdf → (\\S+)\\n  \\[文件\\] 副本\\.pdf，2048 字节，application/pdf → (\\S+)\\n`),
      from,
    );
    expect(m[1]).toMatch(new RegExp(`^${env.consoleFilesDir}/\\d{4}-\\d{2}-\\d{2}/${id}-report\\.pdf$`));
    expect(readFileSync(m[1]!).equals(Buffer.alloc(2048, 7))).toBe(true);
    expect(statSync(m[2]!).size).toBe(2048);
  });

  test("origin uses an IM admin's notify_thread before the console admin's (DESIGN 6.2.4)", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    // An admin who claimed a token in a Telegram DM, written to the gateway database before the gateway starts.
    const db = openDb(env.database);
    new Repo(db, { consoleEnabled: true, classifierEnabledDefault: true, platforms: new Set(["telegram", "console"]) }).putAdmin({
      identity: "telegram:42",
      displayName: "甲",
      notifyThread: "telegram:42",
      addedBy: "console:operator",
    });
    db.$client.close();
    await start({}, env);
    const { peer, hello } = await connectBridge(env);
    track(peer);
    expect(hello.admins).toEqual(["telegram:42", "console:operator"]);
    expect(await peer.request("origin", { requesters: null, thread: null })).toEqual({ target: "telegram:42" });
    expect(await peer.request("origin", { requesters: ["console:operator"], thread: null })).toEqual({ target: "console:main" });
    expect(await peer.request("origin", { requesters: ["telegram:42"], thread: "console:main" })).toEqual({ target: "console:main" });
  });

  test("an image whose inbox file the agent replaced is sent again without the replacement's content", async () => {
    const { env, gw } = await start({ imageMaxBytes: 1000 });
    track(await connectDaemon(env));
    writeFileSync(join(env.workspace, "p.png"), PNG);
    writeFileSync(join(env.root, "gateway.json.copy"), '{"platforms":{"telegram":{"botToken":"123:secret"}}}');
    const first = await connectBridge(env);
    const texts = ["第一张", "第二张", "第三张", "第四张"];
    for (const t of texts) gw.type(`:file p.png ${t}`);
    await first.peer.waitMessage((x) => x.text === "第四张");
    const sent = texts.map((t) => first.peer.messages().find((x) => x.text === t));
    for (const m of sent) {
      expect(Buffer.from(m.attachments[0].data, "base64").equals(PNG)).toBe(true);
    }
    // Nothing was acked, so the next hello sends all four again. Meanwhile the agent changes the inbox files.
    await first.peer.close();
    const inboxFile = (i: number) => join(env.workspace, sent[i].attachments[0].path);
    unlinkSync(inboxFile(0));
    symlinkSync(join(env.root, "gateway.json.copy"), inboxFile(0));
    unlinkSync(inboxFile(1));
    execFileSync("mkfifo", [inboxFile(1)]);
    writeFileSync(inboxFile(2), Buffer.alloc(1001, 1));
    unlinkSync(inboxFile(3));

    const second = await connectBridge(env);
    track(second.peer);
    await second.peer.waitMessage((x) => x.text === "第四张");
    const again = texts.map((t) => second.peer.messages().find((x) => x.text === t).attachments[0]);
    const withoutData = (i: number, extra: object) => {
      const { data: _data, ...saved } = sent[i].attachments[0];
      return { ...saved, ...extra };
    };
    expect(again).toEqual([
      // A symlink to a file of bearbot-gw and a FIFO are not read.
      withoutData(0, { error: "文件已被替换，没有读取" }),
      withoutData(1, { error: "文件已被替换，没有读取" }),
      // image_max_bytes applies to the file as it is now.
      withoutData(2, {}),
      withoutData(3, { error: "文件已不存在" }),
    ]);
  });

  test("attachments are not written through symlinks the agent put into the inbox", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    writeFileSync(join(env.workspace, "a.txt"), "附件内容");
    // Files of bearbot-gw outside the workspace, for example gateway.db.
    const db = join(env.root, "outside.db");
    writeFileSync(db, "管理员与绑定");
    const outsideDir = join(env.root, "outside-dir");
    mkdirSync(outsideDir);

    gw.type("first");
    const m1 = await peer.waitMessage((x) => x.text === "first");
    // Console message ids are "<start>-<n>"; the next input gets n + 1 (no output was sent in between).
    const [base, n] = m1.id.split("-");
    const nextId = `${base}-${Number(n) + 1}`;
    const date = gatewayDate();
    mkdirSync(join(env.workspace, "inbox", date), { recursive: true });
    symlinkSync(db, join(env.workspace, "inbox", date, `${nextId}-a.txt`));
    gw.type(":file a.txt 第二条");
    const m2 = await peer.waitMessage((x) => x.text === "第二条");
    expect(m2.id).toBe(nextId);
    expect(m2.attachments).toEqual([{ type: "file", name: "a.txt", size: Buffer.byteLength("附件内容"), mimeType: "text/plain", path: `inbox/${date}/${nextId}-a.txt` }]);
    expect(readFileSync(db, "utf8")).toBe("管理员与绑定");
    const saved = join(env.workspace, m2.attachments[0].path);
    expect(lstatSync(saved).isFile()).toBe(true);
    expect(readFileSync(saved, "utf8")).toBe("附件内容");
    expect(readdirSync(join(env.workspace, "inbox", date))).toEqual([`${nextId}-a.txt`]);

    // The inbox directory itself replaced by a symlink.
    rmSync(join(env.workspace, "inbox"), { recursive: true });
    symlinkSync(outsideDir, join(env.workspace, "inbox"));
    gw.type(":file a.txt 第三条");
    const m3 = await peer.waitMessage((x) => x.text === "第三条");
    expect(m3.attachments).toHaveLength(1);
    expect(m3.attachments[0].path).toBeUndefined();
    expect(m3.attachments[0].error).toMatch(/inbox 不是目录，附件没有保存$/);
    expect(readdirSync(outsideDir)).toEqual([]);
  });

  test("a directory swapped for a symlink while a file is being sent does not lead outside the workspace", async () => {
    const { env, gw } = await start();
    const { peer } = await connectBridge(env);
    track(peer);
    const outside = join(env.root, "outside");
    mkdirSync(outside);
    writeFileSync(join(outside, "f.txt"), "SECRET-SECRET-SECRET");
    mkdirSync(join(env.workspace, "a"));
    writeFileSync(join(env.workspace, "a", "f.txt"), "inside");
    // Another process keeps swapping workspace/a between the real directory and a symlink to `outside`.
    const swapper = spawn(
      process.execPath,
      [
        "-e",
        `const fs = require("node:fs"); const [ws, outside] = process.argv.slice(1);
         for (;;) { fs.renameSync(ws + "/a", ws + "/a.real"); fs.symlinkSync(outside, ws + "/a"); fs.unlinkSync(ws + "/a"); fs.renameSync(ws + "/a.real", ws + "/a"); }`,
        env.workspace,
        outside,
      ],
      { stdio: "ignore" },
    );
    const swapperExited = new Promise((r) => swapper.on("exit", r));
    cleanups.push(async () => {
      swapper.kill("SIGKILL");
      await swapperExited;
    });

    let sent = 0;
    for (let i = 0; i < 1000; i++) {
      try {
        await peer.request("post", { target: "console:main", markdown: `第 ${i} 次`, files: [{ path: "a/f.txt" }] });
        sent += 1;
      } catch (e) {
        expect((e as Error).message).toMatch(/文件不在工作区目录内：a\/f\.txt|无法读取文件 a\/f\.txt/);
      }
    }
    expect(sent).toBeGreaterThan(0);
    await peer.request("post", { target: "console:main", markdown: "结束" });
    await gw.waitConsole(/\n结束\n/);
    const sizes = [...gw.consoleOut.matchAll(/\[文件\] f\.txt，(\d+) 字节/g)].map((m) => Number(m[1]));
    expect(sizes).toHaveLength(sent);
    expect(new Set(sizes)).toEqual(new Set([6]));
  });

  test("sending to the console fails once its output is gone; platforms.status shows the error", async () => {
    const { env, gw } = await start();
    const ctl = track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    await peer.request("post", { target: "console:main", markdown: "visible" });
    await gw.waitConsole(/visible\n/);
    // The reader of bearbotd's standard output went away.
    (gw.child.stdio[4] as Readable).destroy();
    expect((await peer.requestError("post", { target: "console:main", markdown: "lost-1" })).message).toMatch(/^发送失败：/);
    expect((await peer.requestError("post", { target: "console:main", markdown: "lost-2" })).message).toMatch(
      /^发送失败：控制台输出已失败：/,
    );
    const { platforms } = await ctl.request("platforms.status", {});
    expect(platforms).toEqual([expect.objectContaining({ name: "console", state: "line", lastError: expect.any(String), lastErrorAt: expect.any(String) })]);
    const history = await peer.request("history", { target: "console:main", limit: 10 });
    expect(history.messages.map((x: any) => [x.direction, x.text])).toEqual([["out", "visible"]]);
    // Input and the other directions keep working.
    gw.type("still typing");
    await peer.waitMessage((x) => x.text === "still typing");
  });
});

describe("gateway-ctl.sock", () => {
  test("second connection refused, tokens, admins, notify", async () => {
    const { env, gw } = await start();
    const ctl = track(await connectDaemon(env));
    const second = await new Promise<string>((resolve) => {
      const s = connect(env.ctlSocket);
      s.on("close", () => resolve("closed"));
      s.on("error", () => resolve("closed"));
    });
    expect(second).toBe("closed");
    await gw.waitLog(/second gateway-ctl.sock connection refused/);

    const { peer } = await connectBridge(env);
    track(peer);

    const created = await ctl.request("admin.createToken", { ttlMs: 60_000 });
    expect(created.token).toMatch(/^bearbot-[a-z2-7]{26}$/);
    expect(created.expiresAt).toMatch(ISO_LOCAL);
    const from = nthOf(gw);
    gw.type(`  ${created.token}  `);
    await gw.waitConsole(/控制台用户是内置管理员，不需要领取令牌/, from);
    gw.type("after token");
    await peer.waitMessage((x) => x.text === "after token");
    expect(peer.messages().map((x) => x.text)).toEqual(["after token"]);

    expect(await ctl.request("admin.list", {})).toEqual({
      admins: [{ identity: "console:operator", displayName: "控制台", notifyThread: "console:main", addedBy: null, addedAt: null, builtin: true }],
    });
    const builtin = await ctl.requestError("admin.revoke", { identity: "console:operator" });
    expect(builtin).toEqual({ code: -32000, message: "内置管理员不能撤销" });
    expect((await ctl.requestError("admin.revoke", { identity: "telegram:5" })).message).toMatch(/不是管理员/);

    gw.type("/bb admin add telegram:42");
    await gw.waitConsole(/已添加管理员 telegram:42/, from);
    await peer.waitNotification("admins.changed", (p) => p.admins.length === 2);
    const list = await ctl.request("admin.list", {});
    expect(list.admins.map((a: any) => [a.identity, a.builtin, a.addedBy])).toEqual([
      ["telegram:42", false, "console:operator"],
      ["console:operator", true, null],
    ]);
    expect(await ctl.request("admin.revoke", { identity: "telegram:42" })).toEqual({});
    await peer.waitNotification("admins.changed", (p) => p.admins.length === 1);

    expect(await ctl.request("notify", { text: "管理员通知：故障 inc-1" })).toEqual({});
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #\\d+\\]\\n管理员通知：故障 inc-1\\n`), from);

    expect((await ctl.requestError("agent.state", { state: "bogus" })).code).toBe(-32602);
    expect((await ctl.requestError("nosuch", {})).code).toBe(-32601);
  });

  test("drain stops reading the console, sending keeps working, SIGTERM exits 0", async () => {
    const { env, gw } = await start();
    const ctl = track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    gw.type("before drain");
    await peer.waitMessage((x) => x.text === "before drain");
    expect(await ctl.request("drain", {})).toEqual({});
    gw.type("after drain");
    await sleep(500);
    expect(peer.messages().map((x) => x.text)).toEqual(["before drain"]);
    const from = nthOf(gw);
    await peer.request("post", { target: "console:main", markdown: "最终回复" });
    await gw.waitConsole(/最终回复\n/, from);
    gw.child.kill("SIGTERM");
    await gw.exited;
    expect(gw.exitCode).toBe(0);
    expect(gw.logOut).toMatch(/"msg":"gateway stopped"/);
    expect(existsSync(env.database)).toBe(true);
  });
});

describe("/bb commands from the console", () => {
  async function setup() {
    const { env, gw } = await start();
    const ctl = track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    return { env, gw, ctl, peer };
  }

  test("attach changes and persists the console binding, detach is refused, bindings.changed", async () => {
    const { env, gw, peer } = await setup();
    gw.type("/bb attach --reply each --interrupt steer");
    await gw.waitConsole(/已更新绑定 console:main（控制台）：trigger=all，reply=each，interrupt=steer/);
    const changed = await peer.waitNotification("bindings.changed");
    expect(changed.bindings[0]).toMatchObject({ channel: "console:main", replyMode: "each", interrupt: "steer" });
    gw.type("/bb attach --trigger prefix");
    await gw.waitConsole(/trigger 为 prefix 时需要用 --prefix 指定前缀/);
    gw.type("/bb attach --trigger bogus");
    await gw.waitConsole(/--trigger 的取值只能是 mention、all、prefix/);
    gw.type("/bb attach --foo");
    await gw.waitConsole(/多余的参数：--foo/);
    gw.type("/bb detach");
    await gw.waitConsole(/控制台频道始终绑定/);
    gw.type("/bb here");
    await gw.waitConsole(/频道：console:main（控制台）\n会话串：console:main\n私聊：是\n绑定：trigger=all，reply=each，interrupt=steer\nagent 状态：running/);

    // Prefix trigger strips the prefix.
    gw.type("/bb attach --trigger prefix --prefix '!bot'");
    await gw.waitConsole(/trigger=prefix，prefix=!bot/);
    gw.type("!bot 你好");
    gw.type("no prefix");
    await peer.waitMessage((x) => x.text === "no prefix");
    expect(peer.messages().map((x) => [x.text, x.trigger])).toEqual([
      ["你好", true],
      ["no prefix", false],
    ]);

    await gw.stop();
    const again = new GatewayProcess(env);
    cleanups.push(() => again.kill());
    await again.ready();
    const second = await connectBridge(env);
    track(second.peer);
    expect(second.hello.bindings[0]).toMatchObject({ trigger: "prefix", prefix: "!bot", replyMode: "each", interrupt: "steer" });
  });

  test("admin list/add/remove", async () => {
    const { gw, peer } = await setup();
    gw.type("/bb admin list");
    await gw.waitConsole(/管理员：\n  console:operator（控制台）：内置，通知会话串 console:main/);
    gw.type("/bb admin remove console:operator");
    await gw.waitConsole(/内置管理员 console:operator 不能移除/);
    gw.type("/bb admin add console:operator");
    await gw.waitConsole(/console:operator 是内置管理员，不需要添加/);
    gw.type("/bb admin add nonsense");
    await gw.waitConsole(/身份格式不正确：nonsense/);
    gw.type("/bb admin add");
    await gw.waitConsole(/用法：\/bb admin add <身份>/);
    gw.type("/bb admin add qq:OPENID");
    await gw.waitConsole(/已添加管理员 qq:OPENID/);
    gw.type("/bb admin add qq:OPENID");
    await gw.waitConsole(/qq:OPENID 已经是管理员/);
    gw.type("/bb admin list");
    await gw.waitConsole(/  qq:OPENID：通知会话串 无，由 console:operator 添加于 [\s\S]*?\n  console:operator（控制台）：内置/);
    gw.type("/bb admin remove qq:OPENID");
    await gw.waitConsole(/已移除管理员 qq:OPENID/);
    gw.type("/bb admin remove qq:OPENID");
    await gw.waitConsole(/qq:OPENID 不是管理员/);
    await peer.waitNotification("admins.changed", (p) => p.admins.length === 1);
  });

  test("classifier on/off/allow/deny/clear/status with classifier.changed", async () => {
    const { env, gw, peer } = await setup();
    gw.type("/bb classifier status");
    await gw.waitConsole(/classifier：开启（配置文件默认值）\n规则：\n  （无）\nCLASSIFIER\.md：不存在/);
    gw.type("/bb classifier off");
    await gw.waitConsole(/classifier 已关闭/);
    expect(await peer.waitNotification("classifier.changed", (p) => p.enabled === false)).toEqual({ enabled: false, rules: [] });
    gw.type("/bb classifier allow bash 'mcp:*' my_tool");
    await gw.waitConsole(/已添加放行规则：bash、mcp:\*、my_tool/);
    gw.type("/bb classifier deny web_*");
    await gw.waitConsole(/已添加拒绝规则：web_\*/);
    const last = await peer.waitNotification("classifier.changed", (p) => p.rules.length === 4);
    expect(last.rules).toEqual([
      { pattern: "bash", action: "allow" },
      { pattern: "mcp:*", action: "allow" },
      { pattern: "my_tool", action: "allow" },
      { pattern: "web_*", action: "deny" },
    ]);
    gw.type("/bb classifier clear my_tool nothere");
    await gw.waitConsole(/已删除规则：my_tool\n没有这些规则：nothere/);
    writeFileSync(join(env.workspace, "CLASSIFIER.md"), "# 规则\n");
    gw.type("/bb classifier on");
    await gw.waitConsole(/classifier 已开启/);
    gw.type("/bb classifier status");
    await gw.waitConsole(
      /classifier：开启（数据库设置，console:operator 修改于 [^）]+）\n规则：\n  allow bash\n  allow mcp:\*\n  deny web_\*\nCLASSIFIER\.md：存在（普通文件），9 字节，修改时间 /,
    );
    gw.type("/bb classifier allow");
    await gw.waitConsole(/用法：\/bb classifier allow <工具模式…>/);
  });

  test("attach takes an option value that starts with '-'; a missing or repeated value is refused", async () => {
    const { gw, peer } = await setup();
    gw.type("/bb attach --trigger prefix --prefix -ai");
    await gw.waitConsole(/已更新绑定 console:main（控制台）：trigger=prefix，prefix=-ai，reply=final，interrupt=queue\n/);
    gw.type("-ai 你好");
    expect(await peer.waitMessage((x) => x.text === "你好")).toMatchObject({ trigger: true });
    gw.type("/bb attach --reply");
    await gw.waitConsole(/参数 --reply 需要一个值\n/);
    gw.type("/bb attach --prefix=");
    await gw.waitConsole(/参数 --prefix 需要一个值\n/);
    gw.type("/bb attach --reply each --reply final");
    await gw.waitConsole(/参数 --reply 只能给一次\n/);
    gw.type("/bb here --reply");
    await gw.waitConsole(/\/bb here 不接受参数 --reply\n/);
    gw.type("/bb detach --prefix=x");
    await gw.waitConsole(/\/bb detach 不接受参数 --prefix\n/);
  });

  test("help, unknown commands and usage errors need no bearbotd; compact and repair notes keep option words", async () => {
    const { gw, ctl, peer } = await setup();
    gw.type("/bb snapshot diff");
    await gw.waitConsole(/用法：\/bb snapshot diff <引用> \[引用\]/);
    gw.type("/bb help");
    await gw.waitConsole(/bearbot 管理命令（只有管理员可以使用）：/);
    gw.type("/bb nosuch");
    await gw.waitConsole(/未知的命令：\/bb nosuch，发送 \/bb help 查看命令列表/);
    expect(ctl.requests).toEqual([]);

    // The requests the gateway sends to bearbotd (recorded, not answered here; daemon/e2e checks the answers).
    gw.type("/bb compact 保留 --reply 记忆");
    expect(await ctl.waitRequest("session")).toEqual({ action: "compact", instructions: "保留 --reply 记忆" });
    gw.type("/bb repair 检查 --prefix=x 参数");
    expect(await ctl.waitRequest("repair")).toEqual({ note: "检查 --prefix=x 参数" });
    gw.type("/bb rollback -x");
    expect(await ctl.waitRequest("rollback")).toEqual({ requester: "console:operator", thread: "console:main", snapshot: "-x" });
    expect(peer.messages()).toHaveLength(0);
  });

  test("commands answer that bearbotd is not connected when the ctl connection is missing", async () => {
    const { gw } = await start();
    gw.type("/bb new");
    await gw.waitConsole(/bearbotd 没有连接 gateway，请稍后再试/);
  });
});

describe("!bb from the console", () => {
  test("the question goes to bearbot-im as a btw request and the answer to the thread; !bb never reaches message_log", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    const { peer } = await connectBridge(env);
    track(peer);
    const from = nthOf(gw);

    gw.type("!bb   你在做什么");
    const first = await peer.waitRequestEntry("btw");
    expect(first.params).toEqual({ question: "你在做什么", thread: "console:main", sender: { identity: "console:operator", name: "控制台" } });
    peer.respond(first.id, { result: { answer: "正在运行 **测试**" } });
    await gw.waitConsole(new RegExp(`\\[${CLOCK} bearbot #\\d+\\]\\n正在运行 \\*\\*测试\\*\\*\\n`), from);

    gw.type("!BB@bearbot 进度如何");
    const second = await peer.waitRequestEntry("btw", 1);
    expect(second.params.question).toBe("进度如何");
    peer.respond(second.id, { error: "pi 的运行时当前不可用" });
    await gw.waitConsole(/btw 提问失败：pi 的运行时当前不可用/, from);

    gw.type("!bb@otherbot 不是问 bearbot 的");
    gw.type("!bb");
    await gw.waitConsole(/用法：!bb <问题>，在不打断 agent 的情况下询问它正在做什么/, from);
    gw.type("!bbq 普通消息");
    await peer.waitMessage((m) => m.text === "!bbq 普通消息");
    expect(peer.messages().map((m) => m.text)).toEqual(["!bbq 普通消息"]);
    expect(peer.requests.filter((r) => r.method === "btw")).toHaveLength(2);
    const history = await peer.request("history", { target: "console:main", limit: 20 });
    expect(history.messages.filter((m: any) => m.direction === "in").map((m: any) => m.text)).toEqual(["!bbq 普通消息"]);
  });

  test("without bearbot-im, and when bearbot-im disconnects before answering", async () => {
    const { env, gw } = await start();
    track(await connectDaemon(env));
    gw.type("!bb 在吗");
    await gw.waitConsole(/bearbot-im 没有连接 gateway（agent 状态：running），无法回答/);

    const { peer } = await connectBridge(env);
    const from = nthOf(gw);
    gw.type("!bb 在吗");
    await peer.waitRequestEntry("btw");
    await peer.close();
    await gw.waitConsole(/btw 提问失败：gateway\.sock 连接已断开，请求 btw 没有得到响应/, from);
  });
});

describe("files, sockets, retention, webhook server", () => {
  test("socket modes: gateway.sock 0660 with the configured group, gateway-ctl.sock 0600", async () => {
    const { env } = await start();
    const gwSock = statSync(env.gatewaySocket);
    expect(gwSock.isSocket()).toBe(true);
    expect(gwSock.mode & 0o777).toBe(0o660);
    expect(gwSock.gid).toBe(process.getgid!());
    expect(statSync(env.ctlSocket).mode & 0o777).toBe(0o600);
  });

  test("a stale socket file is replaced; another file at the socket path stops the gateway", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    const first = await start({}, env);
    await first.gw.kill();
    expect(existsSync(env.gatewaySocket)).toBe(true);
    const second = await start({}, env);
    await second.gw.stop();
    writeFileSync(env.ctlSocket, "not a socket");
    const third = new GatewayProcess(env);
    cleanups.push(() => third.kill());
    await third.exited;
    expect(third.exitCode).not.toBe(0);
    expect(third.logOut).toMatch(/exists and is not a socket/);
  });

  test("retention removes old message_log rows and old console-files date directories", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    const first = await start({}, env);
    first.gw.type("will be purged");
    await waitFor("row", () => (/"Direct message received/.test(first.gw.logOut) ? true : undefined));
    await sleep(200);
    await first.gw.stop();

    mkdirSync(join(env.consoleFilesDir, "2020-01-01"), { recursive: true });
    writeFileSync(join(env.consoleFilesDir, "2020-01-01", "1-old.txt"), "old");
    // Today in the gateway's time zone, which can differ from the zone of this process.
    const todayDir = gatewayDate();
    mkdirSync(join(env.consoleFilesDir, todayDir), { recursive: true });
    env.config.messageRetentionMs = 1000;
    writeConfig(env);
    await sleep(1100);

    const second = await start({}, env);
    await second.gw.waitLog(/"msg":"retention purge"/);
    expect(existsSync(join(env.consoleFilesDir, "2020-01-01"))).toBe(false);
    expect(existsSync(join(env.consoleFilesDir, todayDir))).toBe(true);
    const { peer } = await connectBridge(env);
    track(peer);
    expect((await peer.request("history", { target: "console:main", limit: 10 })).messages).toEqual([]);
  });

  test("webhook server answers 404 for platforms without webhooks", async () => {
    const { gw } = await start({ http: { listen: "127.0.0.1:0" } });
    await gw.waitLog(/"msg":"webhook HTTP server listening"/);
    const port = Number(/"port":(\d+)/.exec(gw.logOut.split("webhook HTTP server listening")[0]!.split("\n").at(-1)!)![1]);
    for (const p of ["console", "telegram", "nosuch"]) {
      const res = await fetch(`http://127.0.0.1:${port}/webhooks/${p}`, { method: "POST", body: "{}" });
      expect(res.status).toBe(404);
    }
  });
});

describe("console disabled", () => {
  test("no console binding, no builtin admin, console:main is no target", async () => {
    const env = makeEnv();
    cleanups.push(() => removeEnv(env));
    delete env.config.console;
    writeConfig(env);
    const gw = new GatewayProcess(env, { console: false });
    cleanups.push(() => gw.kill());
    await gw.ready();
    const ctl = track(await connectDaemon(env));
    const { peer, hello } = await connectBridge(env);
    track(peer);
    expect(hello).toEqual({ bindings: [], admins: [], classifier: { enabled: true, rules: [] }, self: {} });
    expect((await peer.requestError("post", { target: "console:main", markdown: "x" })).message).toMatch(/不是已绑定的频道/);
    expect((await peer.request("targets", {})).targets).toEqual([]);
    expect(await ctl.request("admin.list", {})).toEqual({ admins: [] });
    expect(await ctl.request("platforms.status", {})).toEqual({ platforms: [] });
    expect(await peer.request("origin", { requesters: null, thread: null })).toEqual({ target: null });
  });
});
