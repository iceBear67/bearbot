// Telegram updates through the real Telegram adapter of the gateway, fed with processUpdate(update,
// { deduplicate: false }), the call its polling loop makes for every update (chat-platforms.md 5.2), on a real Chat
// with the gateway's Ingest, Commands, Repo and database. No network: the Bot API base URL points at a closed local
// port, so getMe, polling and replies fail and are only logged.
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import type { TelegramMessage, TelegramUpdate, TelegramUser } from "@chat-adapter/telegram";
import { Chat, type WebhookOptions } from "chat";
import { afterAll, beforeAll, beforeEach, expect, test } from "vitest";
import { AgentState } from "../src/agent-state.ts";
import { BridgeServer } from "../src/bridge-server.ts";
import { Commands } from "../src/commands.ts";
import { parseConfig } from "../src/config.ts";
import { CtlServer } from "../src/ctl-server.ts";
import { openDb, type Db } from "../src/db/index.ts";
import { Ingest } from "../src/ingest.ts";
import { createLogger, toChatLogger } from "../src/log.ts";
import { MessageLog } from "../src/message-log.ts";
import { Outbound } from "../src/outbound.ts";
import { Platforms } from "../src/platforms.ts";
import { Repo } from "../src/repo.ts";
import { createSqliteState } from "../src/state/sqlite-state.ts";
import { Tokens } from "../src/tokens.ts";

type UpdateFeed = { processUpdate(update: TelegramUpdate, options?: WebhookOptions): Promise<void>[] | void };

let root: string;
let db: Db;
let repo: Repo;
let log: MessageLog;
let tokens: Tokens;
let platforms: Platforms;
let bot: Chat;
let updateId = 1;
let messageId = 1;

beforeAll(async () => {
  process.env.TELEGRAM_API_BASE_URL = "http://127.0.0.1:9";
  root = mkdtempSync("/tmp/bbtg");
  mkdirSync(join(root, "ws"));
  const cfg = parseConfig(
    "telegram-ingest",
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
      platforms: { telegram: { botToken: "123456:offline-test-token" } },
    }),
  );
  // The same objects main.ts builds; the sockets are not opened, so bearbotd counts as not connected.
  const logger = createLogger("silent");
  const chatLogger = toChatLogger(logger);
  db = openDb(cfg.database);
  log = new MessageLog(db);
  platforms = new Platforms(cfg, logger, chatLogger);
  repo = new Repo(db, { consoleEnabled: false, classifierEnabledDefault: true, platforms: platforms.names() });
  tokens = new Tokens(db);
  const agentState = new AgentState(repo);
  agentState.update("running", null);
  const outbound = new Outbound(platforms, log, repo, logger, cfg.workspaceDir);
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
});

afterAll(async () => {
  await platforms.stopReceiving();
  await bot.shutdown();
  db.$client.close();
  rmSync(root, { recursive: true, force: true });
});

beforeEach(() => {
  for (const identity of repo.adminIdentities()) repo.removeAdmin(identity);
  repo.setClassifierEnabled(true, "test");
});

/** Feeds one message update and waits for the handlers. */
async function feed(message: Omit<TelegramMessage, "message_id" | "date"> & { message_id?: number }): Promise<TelegramMessage> {
  const full: TelegramMessage = { message_id: messageId++, date: Math.floor(Date.now() / 1000), ...message };
  const tasks = (platforms.telegram as unknown as UpdateFeed).processUpdate({ update_id: updateId++, message: full }, { deduplicate: false });
  await Promise.all(tasks ?? []);
  return full;
}

/** A message that starts with a bot command entity, as Telegram sends it for "/bb ...". */
function command(text: string): Pick<TelegramMessage, "text" | "entities"> {
  return { text, entities: [{ type: "bot_command", offset: 0, length: 3 }] };
}

const ALICE: TelegramUser = { id: 111, is_bot: false, first_name: "Alice" };
const TOPIC_CREATOR: TelegramUser = { id: 999, is_bot: false, first_name: "Creator" };
const BOB: TelegramUser = { id: 555, is_bot: false, first_name: "Bob" };
// The fake senders Telegram puts into `from` for messages sent on behalf of a chat; the same in every chat.
const GROUP_ANONYMOUS_BOT: TelegramUser = { id: 1087968824, is_bot: true, first_name: "Group", username: "GroupAnonymousBot" };
const CHANNEL_BOT: TelegramUser = { id: 136817688, is_bot: true, first_name: "Channel", username: "Channel_Bot" };
const FORUM = { id: -100777, type: "supergroup" as const, title: "forum", is_forum: true };

/** Admin identities, sorted (admins added within the same millisecond have no fixed order). */
function admins(): string[] {
  return repo.adminIdentities().sort();
}

test("in a forum topic only an explicit reply selects the user for /bb admin add", async () => {
  repo.putAdmin({ identity: "telegram:111", displayName: "Alice", notifyThread: "telegram:111", addedBy: "test" });
  const root = { message_id: 7, date: 1, chat: FORUM, from: TOPIC_CREATOR, text: "topic" };
  // Not a reply: Telegram still sets reply_to_message to the topic's first message.
  const inTopic = { chat: FORUM, from: ALICE, is_topic_message: true, message_thread_id: 7, reply_to_message: root };
  await feed({ ...inTopic, ...command("/bb admin add") });
  await feed({ ...inTopic, text: "/bb@bearbot admin add" });
  expect(admins()).toEqual(["telegram:111"]);

  const reply = { message_id: 20, date: 1, chat: FORUM, from: BOB, is_topic_message: true, message_thread_id: 7, reply_to_message: root, text: "hi" };
  await feed({ ...inTopic, reply_to_message: reply, ...command("/bb admin add") });
  expect(admins()).toEqual(["telegram:111", "telegram:555"]);
});

test("a message sent on behalf of a chat has the chat as sender, not the shared fake user", async () => {
  const legit = { id: -100111, type: "supergroup" as const, title: "legit" };
  const other = { id: -100222, type: "supergroup" as const, title: "attacker" };

  // An admin of "legit" claims a token while posting anonymously.
  const { token } = tokens.create(600_000);
  await feed({ chat: legit, sender_chat: legit, from: GROUP_ANONYMOUS_BOT, text: token });
  expect(admins()).toEqual(["telegram:chat:-100111"]);
  expect(repo.adminRow("telegram:chat:-100111")).toMatchObject({ displayName: "legit", notifyThread: "telegram:-100111" });

  // An anonymous admin of another group is not that admin: no /bb, as a slash command or as plain text.
  await feed({ chat: other, sender_chat: other, from: GROUP_ANONYMOUS_BOT, ...command("/bb classifier off") });
  await feed({ chat: other, sender_chat: other, from: GROUP_ANONYMOUS_BOT, text: "/bb@bearbot classifier off" });
  expect(repo.classifier().enabled).toBe(true);
  await feed({ chat: legit, sender_chat: legit, from: GROUP_ANONYMOUS_BOT, ...command("/bb classifier off") });
  expect(repo.classifier().enabled).toBe(false);

  // Reply-based /bb admin add on an anonymous message adds that chat.
  repo.putAdmin({ identity: "telegram:111", displayName: "Alice", notifyThread: "telegram:111", addedBy: "test" });
  const anonymous = { message_id: 30, date: 1, chat: other, sender_chat: other, from: GROUP_ANONYMOUS_BOT, text: "我是匿名管理员" };
  await feed({ chat: other, from: ALICE, reply_to_message: anonymous, ...command("/bb admin add") });
  expect(admins()).toEqual(["telegram:111", "telegram:chat:-100111", "telegram:chat:-100222"]);

  // In a bound channel: sender, role and replyTo come from sender_chat.
  repo.putBinding({
    channelId: "telegram:-100111",
    platform: "telegram",
    channelName: "legit",
    trigger: "all",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
  const channel = { id: -100999, type: "channel" as const, title: "公告频道" };
  const post = { message_id: 40, date: 1, chat: legit, sender_chat: channel, from: CHANNEL_BOT, text: "频道公告" };
  await feed({ chat: legit, sender_chat: legit, from: GROUP_ANONYMOUS_BOT, reply_to_message: post, text: "大家好" });
  const row = log.inboundAfter(0, 10).find((r) => r.payload.text === "大家好")!;
  expect(row.payload.sender).toEqual({ id: "telegram:chat:-100111", name: "legit", role: "admin", isBot: false });
  expect(row.payload.replyTo).toEqual({ id: "-100111:40", sender: "telegram:chat:-100999", name: "公告频道", text: "频道公告" });
});
