// Slack events through the real Slack adapter of the gateway, fed with routeSocketEvent(body, "events_api", ack), the
// call its socket mode loop makes for every envelope, on a real Chat with the gateway's Ingest, Commands, Repo and
// database. The tokens are made up, so the adapter's own Web API calls fail and are only logged; `chat` is unset on
// the adapter until initialize(), which is why the test sets `botUserId` and lets the adapter parse users by id.
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { Chat, type WebhookOptions } from "chat";
import { afterAll, beforeAll, beforeEach, expect, test } from "vitest";
import { AgentState } from "../src/agent-state.ts";
import { BridgeServer } from "../src/bridge-server.ts";
import { Btw } from "../src/btw.ts";
import { Commands } from "../src/commands.ts";
import { parseConfig } from "../src/config.ts";
import { CtlServer } from "../src/ctl-server.ts";
import { openDb, type Db } from "../src/db/index.ts";
import { Ingest } from "../src/ingest.ts";
import { createLogger, toChatLogger } from "../src/log.ts";
import { MessageLog, type InboundRow } from "../src/message-log.ts";
import { Outbound } from "../src/outbound.ts";
import { Platforms } from "../src/platforms.ts";
import { UserProfiles } from "../src/profiles.ts";
import { Repo } from "../src/repo.ts";
import { createSqliteState } from "../src/state/sqlite-state.ts";
import { Tokens } from "../src/tokens.ts";

/** The protected socket mode entry point, called the way @slack/socket-mode calls it for every envelope. */
type SocketFeed = {
  routeSocketEvent(
    body: Record<string, unknown>,
    eventType: string,
    ack: (response?: Record<string, unknown>) => Promise<void>,
    options?: WebhookOptions,
    retryNum?: number,
  ): Promise<void>;
};

const BOT_USER_ID = "U_BEARBOT";
const BOT_ID = "B_BEARBOT";
const ALICE = "U_ALICE";
const BOB = "U_BOB";
const CHANNEL = "C_BEARBOT_TEST";

let root: string;
let db: Db;
let repo: Repo;
let log: MessageLog;
let tokens: Tokens;
let platforms: Platforms;
let outbound: Outbound;
let bot: Chat;
let eventId = 1;

beforeAll(async () => {
  // Socket mode never connects in the test: the Web API address points at a closed local port, so auth and socket
  // startup fail fast and are only logged by the ConnectLoop.
  process.env.SLACK_API_URL = "http://127.0.0.1:9/";
  root = mkdtempSync("/tmp/bbslack");
  mkdirSync(join(root, "ws"));
  const cfg = parseConfig(
    "slack-ingest",
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
      platforms: { slack: { botToken: "xoxb-not-a-real-token", appToken: "xapp-not-a-real-token" } },
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
  outbound = new Outbound(platforms, log, repo, logger, cfg.workspaceDir);
  const profiles = new UserProfiles({ cfg, platforms, log, logger });
  const bridge = new BridgeServer({ cfg, repo, log, outbound, platforms, profiles, agentState, logger });
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
  const btw = new Btw({ repo, outbound, agentState, asker: bridge, logger });
  const ingest = new Ingest({ cfg, repo, log, tokens, commands, btw, outbound, agentState, delivery: bridge, logger });
  bot = new Chat({ userName: cfg.userName, adapters: platforms.adapters, state: createSqliteState(db), concurrency: "concurrent", logger: chatLogger });
  bot.onDirectMessage((thread, message) => ingest.message(thread, message));
  bot.onNewMention((thread, message) => ingest.message(thread, message));
  bot.onNewMessage(/[\s\S]*/, (thread, message) => ingest.message(thread, message));
  bot.onSlashCommand((event) => ingest.command(event));
  bot.onMessageUpdated((thread, message) => ingest.message(thread, message));
  // initialize() starts the connection loop in the background and never rejects; the socket never connects. The
  // bot user id is what the adapter's own auth.test would have filled in, without which it cannot tell the bot's
  // own messages apart.
  (platforms.slack as unknown as { _botUserId: string })._botUserId = BOT_USER_ID;
  (platforms.slack as unknown as { _botId: string })._botId = BOT_ID;
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

/** A Slack message event body as socket mode delivers it inside `events_api`. */
type SlackEvent = {
  type?: string;
  subtype?: string;
  user?: string;
  bot_id?: string;
  text?: string;
  channel?: string;
  channel_type?: string;
  ts?: string;
  thread_ts?: string;
  edited?: { user: string; ts: string };
  message?: SlackEvent;
  previous_message?: Record<string, unknown>;
};

/** Feeds one socket mode envelope and waits for the handlers. */
async function feed(event: SlackEvent, retryNum?: number): Promise<void> {
  const tasks: Promise<unknown>[] = [];
  const body = { type: "event_callback", team_id: "T_BEARBOT", event_id: `Ev${eventId++}`, event: { type: "message", ...event } };
  await (platforms.slack as unknown as SocketFeed).routeSocketEvent(
    body,
    "events_api",
    async () => {},
    { waitUntil: (task: Promise<unknown>) => void tasks.push(task) },
    retryNum,
  );
  await Promise.all(tasks);
}

/** A member message. */
function message(text: string, extra: Partial<SlackEvent> = {}): SlackEvent {
  const ts = `${Date.now() / 1000}`;
  return { type: "message", user: ALICE, channel: CHANNEL, channel_type: "channel", ts, text, ...extra };
}

function rows(): InboundRow[] {
  return log.inboundAfter(0, 1000).filter((r) => r.payload.platform === "slack");
}

test("a Slack message keeps its thread session, sender and text; leading @bot markup is stripped before /bb detection", async () => {
  repo.putBinding({
    channelId: "slack:C_BEARBOT_TEST",
    platform: "slack",
    channelName: null,
    trigger: "all",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
  const ts = "1720000000.000100";
  await feed(message(`<@${BOT_USER_ID}> 你好`, { ts }));

  const row = rows().find((r) => r.payload.text.includes("你好"))!;
  expect(row.kind).toBe("message");
  expect(row.threadId).toBe(`slack:${CHANNEL}:${ts}`);
  expect(row.payload.channel.id).toBe(`slack:${CHANNEL}`);
  expect(row.payload.sender.id).toBe(`slack:${ALICE}`);
  expect(row.payload.sender.role).toBe("member");
  expect(row.payload.trigger).toBe(true);
});

test("in a thread the session is the thread root, so a reply joins the conversation", async () => {
  repo.putBinding({
    channelId: "slack:C_BEARBOT_TEST",
    platform: "slack",
    channelName: null,
    trigger: "all",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
  const root = "1720000100.000100";
  await feed(message(`<@${BOT_USER_ID}> 第一个问题`, { ts: root }));
  await feed(message("补充一下", { ts: "1720000100.000200", thread_ts: root, user: BOB }));

  const threaded = rows().find((r) => r.payload.text === "补充一下")!;
  expect(threaded.threadId).toBe(`slack:${CHANNEL}:${root}`);
  expect(threaded.payload.sender.id).toBe(`slack:${BOB}`);
});

test("a DM is a DM even when the channel has no thread timestamp", async () => {
  repo.putBinding({
    channelId: "slack:D_DIRECT",
    platform: "slack",
    channelName: null,
    trigger: "mention",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
  await feed({ type: "message", user: ALICE, channel: "D_DIRECT", channel_type: "im", ts: "1720000200.000100", text: "在吗" });
  const row = rows().find((r) => r.payload.text === "在吗")!;
  expect(row.threadId).toBe("slack:D_DIRECT:");
  expect(row.payload.channel.isDM).toBe(true);
  // In a DM a binding with the mention trigger still fires without a mention.
  expect(row.payload.trigger).toBe(true);
});

test("a Slack token claimed by a DM makes the sender an admin; identities are slack:<user id>", async () => {
  const { token } = tokens.create(600_000);
  await feed({ type: "message", user: ALICE, channel: "D_CLAIM", channel_type: "im", ts: "1720000300.000100", text: token });
  expect(repo.adminIdentities()).toEqual([`slack:${ALICE}`]);
  // The token message is not recorded as an inbound message.
  expect(rows().some((r) => r.payload.channel.id === "slack:D_CLAIM")).toBe(false);
});

test("/bb in plain text works without a registered Slack slash command", async () => {
  repo.putAdmin({ identity: `slack:${ALICE}`, displayName: "Alice", notifyThread: "slack:D_ADMIN", addedBy: "test" });
  await feed({ type: "message", user: ALICE, channel: "D_ADMIN", channel_type: "im", ts: "1720000400.000100", text: "/bb classifier off" });
  expect(repo.classifier().enabled).toBe(false);
  // The /bb command is not recorded as an inbound message.
  expect(rows().some((r) => r.payload.channel.id === "slack:D_ADMIN")).toBe(false);
});

test("an edited Slack message is recorded with edited: true and never triggers; the socket retry of a delivery is dropped", async () => {
  repo.putBinding({
    channelId: "slack:C_BEARBOT_TEST",
    platform: "slack",
    channelName: null,
    trigger: "all",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
  const ts = "1720000500.000100";
  await feed({
    type: "message",
    subtype: "message_changed",
    channel: CHANNEL,
    ts: "1720000500.000200",
    message: { type: "message", user: ALICE, ts, text: "改过的内容", edited: { user: ALICE, ts: "1720000501.000000" } },
    previous_message: { type: "message", user: ALICE, ts, text: "原来的内容" },
  });
  const edited = rows().find((r) => r.payload.text === "改过的内容")!;
  expect(edited.payload.edited).toBe(true);
  expect(edited.payload.trigger).toBe(false);

  // The same event delivered again as a retry (retry_num > 0) is dropped by the adapter's event dedupe.
  const twice = message(`<@${BOT_USER_ID}> 只出现一次`, { ts: "1720000500.000300" });
  await feed(twice);
  await feed(twice, 1);
  expect(rows().filter((r) => r.payload.text.includes("只出现一次"))).toHaveLength(1);
});

test("a message from the bot itself is not recorded", async () => {
  await feed({ type: "message", bot_id: BOT_ID, channel: CHANNEL, channel_type: "channel", ts: "1720000600.000100", text: "自己发的" });
  expect(rows().some((r) => r.payload.text === "自己发的")).toBe(false);
});

test("replyTo to a Slack message that is not in message_log fails before anything is sent", async () => {
  repo.putBinding({
    channelId: "slack:C_BEARBOT_REPLY",
    platform: "slack",
    channelName: null,
    trigger: "all",
    prefix: null,
    replyMode: "final",
    interrupt: "queue",
    createdBy: "test",
  });
  const target = "slack:C_BEARBOT_REPLY:1720000700.000100";
  const before = log.inboundAfter(0, 1000).length;
  // The Web API refuses the send, so a successful lookup of a known message would fail with a Slack error instead.
  await expect(outbound.post(target, { text: "回答", replyTo: "1720000799.000000" })).rejects.toThrow(
    "找不到要回复的消息：1720000799.000000",
  );
  expect(log.inboundAfter(0, 1000).length).toBe(before);
  expect(log.history({ threadId: target }, 100).filter((r) => r.direction === "out")).toEqual([]);
});
