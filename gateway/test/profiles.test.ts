// UserProfiles (DESIGN 6.8 `user`, `avatar`) against the real Telegram adapter of the gateway, whose Bot API base
// URL points at a local HTTP server; QQ has no profile interface, so the fallback to message_log is checked, and the
// qlogo URL is checked as a string (the avatar download itself uses the HTTPS-only attachment transport).
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { afterAll, beforeAll, expect, test } from "vitest";
import { parseConfig } from "../src/config.ts";
import { openDb, type Db } from "../src/db/index.ts";
import { createLogger, toChatLogger } from "../src/log.ts";
import { MessageLog } from "../src/message-log.ts";
import { Platforms } from "../src/platforms.ts";
import { qqAvatarUrl, UserProfiles } from "../src/profiles.ts";
import type { InboundPayload } from "../src/types.ts";

/** A 1x1 PNG. */
const PNG = Buffer.from(
  "89504e470d0a1a0a0000000d4948445200000001000000010806000000" + "1f15c4890000000d49444154789c6360000002000154a24f5d0000000049454e44ae426082",
  "hex",
);

let root: string;
let db: Db;
let log: MessageLog;
let platforms: Platforms;
let profiles: UserProfiles;
let server: Server;
let apiBase = "";
/** Requests of the fake Bot API, in order. */
const calls: string[] = [];
/** getChat answers by chat id; ids that are not listed are answered with the Bot API's 400. */
const chats: Record<string, unknown> = {};

beforeAll(async () => {
  server = createServer((req, res) => void handle(req, res));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("fake Bot API did not bind");
  }
  apiBase = `http://127.0.0.1:${address.port}`;
  process.env.TELEGRAM_API_BASE_URL = apiBase;

  root = mkdtempSync("/tmp/bbprof");
  mkdirSync(join(root, "ws"));
  const cfg = parseConfig(
    "profiles",
    JSON.stringify({
      logLevel: "error",
      userName: "bearbot",
      gatewaySocket: join(root, "gw.sock"),
      ctlSocket: join(root, "ctl.sock"),
      socketGroupGid: process.getgid!(),
      database: join(root, "gateway.db"),
      consoleFilesDir: join(root, "console-files"),
      workspaceDir: join(root, "ws"),
      tokenTtlMs: 600000,
      replayWindowMs: 600000,
      messageRetentionMs: 2592000000,
      attachmentTimeoutMs: 60000,
      imageMaxBytes: 5242880,
      maintenanceNotice: "bearbot 正在修复，暂时无法回复。",
      classifierEnabledDefault: true,
      http: { listen: "" },
      platforms: {
        telegram: { botToken: "123456:offline-test-token" },
        qq: { appId: "102000000", clientSecret: "not-a-real-secret", sandbox: true, markdown: true },
      },
    }),
  );
  const logger = createLogger("silent");
  db = openDb(cfg.database);
  log = new MessageLog(db);
  platforms = new Platforms(cfg, logger, toChatLogger(logger));
  profiles = new UserProfiles({ cfg, platforms, log, logger });
});

afterAll(async () => {
  await platforms.stopReceiving();
  db.$client.close();
  await new Promise<void>((resolve) => server.close(() => resolve()));
  rmSync(root, { recursive: true, force: true });
  delete process.env.TELEGRAM_API_BASE_URL;
});

async function body(req: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  const text = Buffer.concat(chunks).toString("utf8");
  return text === "" ? {} : (JSON.parse(text) as Record<string, unknown>);
}

async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = req.url ?? "";
  const method = url.split("/").at(-1) ?? "";
  calls.push(url);
  const reply = (value: unknown): void => {
    const data = JSON.stringify(value);
    res.writeHead(200, { "content-type": "application/json", "content-length": Buffer.byteLength(data) });
    res.end(data);
  };
  if (method === "getChat") {
    const chatId = String((await body(req)).chat_id);
    const chat = chats[chatId];
    if (chat === undefined) {
      reply({ ok: false, error_code: 400, description: "Bad Request: chat not found" });
      return;
    }
    reply({ ok: true, result: chat });
    return;
  }
  if (method === "getUserProfilePhotos") {
    if ((await body(req)).user_id !== "123") {
      reply({ ok: true, result: { total_count: 0, photos: [] } });
      return;
    }
    reply({ ok: true, result: { total_count: 1, photos: [[{ file_id: "small", width: 160, height: 160 }, { file_id: "big", width: 640, height: 640 }]] } });
    return;
  }
  if (method === "getFile") {
    reply({ ok: true, result: { file_id: "big", file_unique_id: "u", file_path: "photos/file_1.png" } });
    return;
  }
  if (url.endsWith("/file/bot123456:offline-test-token/photos/file_1.png")) {
    calls.push("file body");
    res.writeHead(200, { "content-type": "image/png", "content-length": PNG.length });
    res.end(PNG);
    return;
  }
  // The library's own send/reply calls during ingestion are not made here; anything else is a test failure.
  res.writeHead(404, { "content-type": "application/json" });
  res.end(JSON.stringify({ ok: false, error_code: 404, description: "Not Found" }));
}

test("telegram: the profile comes from getChat, the avatar from getUserProfilePhotos", async () => {
  chats["123"] = { id: 123, type: "private", first_name: "Alice", last_name: "Chen", username: "alice" };
  const info = await profiles.info("telegram:123");
  expect(info).toEqual({ identity: "telegram:123", userId: "123", fullName: "Alice Chen", userName: "alice", isBot: false });

  const avatar = await profiles.avatar("telegram:123");
  expect(avatar).toMatchObject({ identity: "telegram:123", mimeType: "image/png", size: PNG.length });
  expect(avatar!.path).toMatch(/^inbox\/\d{4}-\d{2}-\d{2}\/telegram:123-avatar\.png$/);
  expect(readFileSync(join(root, "ws", avatar!.path)).equals(PNG)).toBe(true);
  expect(calls).toContain(`/bot123456:offline-test-token/getUserProfilePhotos`);
  expect(calls).toContain("file body");
});

test("telegram: telegram:chat:<id> is a group, its avatar is the chat photo", async () => {
  chats["-100123"] = {
    id: -100123,
    type: "supergroup",
    title: "测试群",
    username: "test_group",
    photo: { small_file_id: "s", big_file_id: "b" },
  };
  const info = await profiles.info("telegram:chat:-100123");
  expect(info).toEqual({ identity: "telegram:chat:-100123", userId: "-100123", fullName: "测试群", userName: "test_group", isBot: false });

  const avatar = await profiles.avatar("telegram:chat:-100123");
  expect(avatar).toMatchObject({ identity: "telegram:chat:-100123", mimeType: "image/png", size: PNG.length });
});

test("telegram: a user without a profile photo gets null, even when getChat has the chat photo", async () => {
  chats["456"] = { id: 456, type: "private", first_name: "Bob", photo: { small_file_id: "s", big_file_id: "b" } };
  expect(await profiles.avatar("telegram:456")).toBeNull();
});

test("telegram: a chat the bot does not know falls back to the name in message_log", async () => {
  const payload: InboundPayload = {
    kind: "message",
    id: "1-1",
    platform: "telegram",
    channel: { id: "telegram:-100999", isDM: false },
    thread: "telegram:-100999",
    sender: { id: "telegram:999", name: "Bob", role: "member", isBot: false },
    time: "2026-11-02T10:00:00+08:00",
    text: "hi",
    mentionsBot: true,
    edited: false,
    attachments: [],
    trigger: true,
  };
  log.insertInbound({ kind: "message", threadId: payload.thread, channelId: "telegram:-100999", messageId: payload.id, payload, createdAt: Date.now(), quoteId: null });

  expect(await profiles.info("telegram:999")).toEqual({ identity: "telegram:999", userId: "999", fullName: "Bob", userName: "Bob", isBot: false });
  expect(await profiles.avatar("telegram:999")).toBeNull();
});

test("qq: no profile interface, message_log gives the name; the avatar URL is the qlogo workaround", async () => {
  const payload: InboundPayload = {
    kind: "message",
    id: "1-2",
    platform: "qq",
    channel: { id: "qq:group/G1", isDM: false },
    thread: "qq:group/G1",
    sender: { id: "qq:MEMBER_OPENID", name: "小明", role: "member", isBot: false },
    time: "2026-11-02T10:00:00+08:00",
    text: "hi",
    mentionsBot: true,
    edited: false,
    attachments: [],
    trigger: true,
  };
  log.insertInbound({ kind: "message", threadId: payload.thread, channelId: "qq:group/G1", messageId: payload.id, payload, createdAt: Date.now(), quoteId: null });

  expect(await profiles.info("qq:MEMBER_OPENID")).toEqual({
    identity: "qq:MEMBER_OPENID",
    userId: "MEMBER_OPENID",
    fullName: "小明",
    userName: "小明",
    isBot: false,
  });
  expect(qqAvatarUrl("102000000", "MEMBER_OPENID")).toBe("https://q.qlogo.cn/qqapp/102000000/MEMBER_OPENID/640");
});

test("console: the builtin admin has no platform lookup and no avatar", async () => {
  const cfg = parseConfig(
    "profiles-console",
    JSON.stringify({
      logLevel: "error",
      userName: "bearbot",
      gatewaySocket: join(root, "gw2.sock"),
      ctlSocket: join(root, "ctl2.sock"),
      socketGroupGid: process.getgid!(),
      database: join(root, "gateway2.db"),
      consoleFilesDir: join(root, "console-files"),
      workspaceDir: join(root, "ws"),
      tokenTtlMs: 600000,
      replayWindowMs: 600000,
      messageRetentionMs: 2592000000,
      attachmentTimeoutMs: 60000,
      imageMaxBytes: 5242880,
      maintenanceNotice: "bearbot 正在修复，暂时无法回复。",
      classifierEnabledDefault: true,
      http: { listen: "" },
      platforms: {},
      console: { inputFd: 3, outputFd: 4 },
    }),
  );
  const logger = createLogger("silent");
  const consolePlatforms = new Platforms(cfg, logger, toChatLogger(logger));
  const consoleProfiles = new UserProfiles({ cfg, platforms: consolePlatforms, log, logger });
  expect(await consoleProfiles.info("console:operator")).toEqual({
    identity: "console:operator",
    userId: "operator",
    fullName: "控制台",
    userName: "operator",
    isBot: false,
  });
  expect(await consoleProfiles.avatar("console:operator")).toBeNull();
  await consolePlatforms.stopReceiving();
  await expect(consoleProfiles.info("telegram:1")).rejects.toThrow(/平台 telegram 没有启用/);
});
