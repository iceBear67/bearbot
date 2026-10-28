// QQ events through the real QQ adapter of the gateway, fed with handleSocketModePayload(payload), the call its
// socket loop makes for every gateway frame, on a real Chat with the gateway's Ingest, Repo, Outbound and database.
// The credentials are made up, so the adapter's own connection attempts fail and are only logged.
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { Chat } from "chat";
import { afterAll, beforeAll, expect, test } from "vitest";
import { AgentState } from "../src/agent-state.ts";
import { BridgeServer } from "../src/bridge-server.ts";
import { Commands } from "../src/commands.ts";
import { parseConfig } from "../src/config.ts";
import { CtlServer } from "../src/ctl-server.ts";
import { openDb, type Db } from "../src/db/index.ts";
import { Ingest } from "../src/ingest.ts";
import { createLogger, toChatLogger } from "../src/log.ts";
import { MessageLog, type InboundRow } from "../src/message-log.ts";
import { Outbound } from "../src/outbound.ts";
import { Platforms } from "../src/platforms.ts";
import { Repo } from "../src/repo.ts";
import { createSqliteState } from "../src/state/sqlite-state.ts";
import { Tokens } from "../src/tokens.ts";

const GROUP = "qq:group/G_BEARBOT_TEST";

let root: string;
let db: Db;
let log: MessageLog;
let outbound: Outbound;
let platforms: Platforms;
let bot: Chat;
let frame = 1;

beforeAll(async () => {
  root = mkdtempSync("/tmp/bbqq");
  mkdirSync(join(root, "ws"));
  const cfg = parseConfig(
    "qq-ingest",
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
      platforms: { qq: { appId: "102000000", clientSecret: "not-a-real-secret", sandbox: true, markdown: true } },
    }),
  );
  // The same objects main.ts builds; the sockets are not opened, so bearbotd counts as not connected.
  const logger = createLogger("silent");
  const chatLogger = toChatLogger(logger);
  db = openDb(cfg.database);
  log = new MessageLog(db);
  platforms = new Platforms(cfg, logger, chatLogger);
  const repo = new Repo(db, { consoleEnabled: false, classifierEnabledDefault: true, platforms: platforms.names() });
  const tokens = new Tokens(db);
  const agentState = new AgentState(repo);
  agentState.update("running", null);
  outbound = new Outbound(platforms, log, repo, logger, cfg.workspaceDir);
  const bridge = new BridgeServer({ cfg, repo, log, outbound, platforms, agentState, logger });
  const ctl = new CtlServer({
    cfg,
    repo,
    tokens,
    outbound,
    platforms,
    agentState,
    notifier: bridge,
    drain: () => platforms.stopReceiving(),
    logger,
  });
  const commands = new Commands({ cfg, repo, outbound, platforms, agentState, daemon: ctl, notifier: bridge, logger });
  const ingest = new Ingest({ cfg, repo, log, tokens, commands, outbound, agentState, delivery: bridge, logger });
  bot = new Chat({ userName: cfg.userName, adapters: platforms.adapters, state: createSqliteState(db), concurrency: "concurrent", logger: chatLogger });
  bot.onDirectMessage((thread, message) => ingest.message(thread, message));
  bot.onNewMention((thread, message) => ingest.message(thread, message));
  bot.onNewMessage(/[\s\S]*/, (thread, message) => ingest.message(thread, message));
  bot.onSlashCommand((event) => ingest.command(event));
  await bot.initialize();
  repo.putBinding({
    channelId: GROUP,
    platform: "qq",
    channelName: null,
    trigger: "all",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
});

afterAll(async () => {
  await platforms.stopReceiving();
  await bot.shutdown();
  db.$client.close();
  rmSync(root, { recursive: true, force: true });
});

type GroupMessage = { id: string; member: string; content: string; ext: string[]; quoted?: { msgIdx: string; content: string } };

/** Feeds one GROUP_MESSAGE_CREATE frame as QQ sends it and waits for the handlers. */
async function feed(m: GroupMessage): Promise<InboundRow> {
  const d: Record<string, unknown> = {
    id: m.id,
    content: m.content,
    timestamp: new Date().toISOString(),
    group_openid: GROUP.slice("qq:group/".length),
    author: { member_openid: m.member },
    message_scene: { source: "default", ext: m.ext },
    message_type: m.quoted ? 103 : 0,
  };
  if (m.quoted) d.msg_elements = [{ msg_idx: m.quoted.msgIdx, content: m.quoted.content, message_type: 0 }];
  const tasks: Promise<unknown>[] = [];
  const s = frame++;
  await platforms.qq!.handleSocketModePayload({ op: 0, s, t: "GROUP_MESSAGE_CREATE", id: `GROUP_MESSAGE_CREATE:${s}`, d }, {
    waitUntil: (task) => tasks.push(task),
  });
  await Promise.all(tasks);
  return log.inboundAfter(0, 1000).findLast((row) => row.messageId === m.id)!;
}

test("QQ messages keep their quote index; a quote names the quoted message and its sender", async () => {
  const asked = await feed({ id: "ROBOT1.0_alice", member: "M_ALICE", content: "今天天气怎么样", ext: ["msg_idx=REFIDX_alice"] });
  expect(asked.quoteId).toBe("REFIDX_alice");
  expect(asked.payload.replyTo).toBeUndefined();

  const quoting = await feed({
    id: "ROBOT1.0_bob",
    member: "M_BOB",
    content: "我也想知道",
    ext: ["msg_idx=REFIDX_bob", "ref_msg_idx=REFIDX_alice"],
    quoted: { msgIdx: "REFIDX_alice", content: "今天天气怎么样" },
  });
  expect(quoting.quoteId).toBe("REFIDX_bob");
  expect(quoting.payload.replyTo).toEqual({ id: "ROBOT1.0_alice", sender: "qq:M_ALICE", name: "M_ALICE", text: "今天天气怎么样" });

  // A message of the bot, recorded the way Outbound.post records a QQ send with the response's ext_info.ref_idx.
  log.insertOutbound({
    threadId: GROUP,
    channelId: GROUP,
    messageId: "ROBOT1.0_bot",
    payload: { platform: "qq", channel: { id: GROUP, isDM: false }, text: "上海 18℃ 小雨", attachments: [] },
    quoteId: "REFIDX_bot",
  });
  const toBot = await feed({
    id: "ROBOT1.0_carol",
    member: "M_CAROL",
    content: "明天呢",
    ext: ["msg_idx=REFIDX_carol", "ref_msg_idx=REFIDX_bot"],
    quoted: { msgIdx: "REFIDX_bot", content: "上海 18℃ 小雨" },
  });
  expect(toBot.payload.replyTo).toEqual({ id: "ROBOT1.0_bot", sender: "qq:bot", name: "bearbot", text: "上海 18℃ 小雨" });

  // The quoted message is not in message_log (sent before the binding, or purged).
  const unknown = await feed({
    id: "ROBOT1.0_dave",
    member: "M_DAVE",
    content: "这是什么",
    ext: ["msg_idx=REFIDX_dave", "ref_msg_idx=REFIDX_old"],
    quoted: { msgIdx: "REFIDX_old", content: "很早的消息" },
  });
  expect(unknown.payload.replyTo).toEqual({ id: "", sender: "", name: "", text: "很早的消息" });

  // QQ does not give every message a quote index.
  const plain = await feed({ id: "ROBOT1.0_erin", member: "M_ERIN", content: "没有索引", ext: [] });
  expect(plain.quoteId).toBeNull();
});

test("a QQ slash command keeps its quote index", async () => {
  const row = await feed({ id: "ROBOT1.0_cmd", member: "M_ALICE", content: "/weather 上海", ext: ["msg_idx=REFIDX_cmd"] });
  expect(row.kind).toBe("command");
  expect(row.quoteId).toBe("REFIDX_cmd");
});

test("replyTo to a QQ message that is not in message_log fails before anything is sent", async () => {
  await expect(outbound.post(GROUP, { text: "回答", replyTo: "ROBOT1.0_missing" })).rejects.toThrow("找不到要回复的消息：ROBOT1.0_missing");
  expect(log.history({ threadId: GROUP }, 100).filter((row) => row.direction === "out").map((row) => row.messageId)).toEqual(["ROBOT1.0_bot"]);
});
