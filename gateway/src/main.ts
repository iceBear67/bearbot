import { existsSync, readdirSync, rmSync } from "node:fs";
import { join } from "node:path";
import { Chat } from "chat";
import { AgentState } from "./agent-state.ts";
import { BridgeServer } from "./bridge-server.ts";
import { Commands } from "./commands.ts";
import { loadConfigFromArgs, type GatewayConfig } from "./config.ts";
import { CtlServer } from "./ctl-server.ts";
import { openDb, type Db } from "./db/index.ts";
import { HttpServer } from "./http-server.ts";
import { Ingest } from "./ingest.ts";
import { createLogger, toChatLogger, type PinoLogger } from "./log.ts";
import { MessageLog } from "./message-log.ts";
import { Outbound } from "./outbound.ts";
import { Platforms } from "./platforms.ts";
import { Repo } from "./repo.ts";
import { createSqliteState } from "./state/sqlite-state.ts";
import { endOfLocalDay } from "./time.ts";
import { Tokens } from "./tokens.ts";

const RETENTION_INTERVAL_MS = 60 * 60 * 1000;

/** Rows whose attachment downloads were cut off by the previous process are finished with an error. */
function finishInterruptedDownloads(log: MessageLog, logger: PinoLogger): void {
  for (const row of log.pendingInbound()) {
    const payload = row.payload;
    for (const a of payload.attachments) {
      if (a.path === undefined && a.error === undefined) {
        a.error = "gateway 重启，附件没有下载完成";
      }
    }
    delete payload.pending;
    log.updatePayload(row.seq, payload);
    logger.warn({ seq: row.seq }, "attachment download interrupted by a restart");
  }
}

/** message_retention for message_log and the console-files date directories (DESIGN 6.2.4, 6.4). */
function purgeRetention(cfg: GatewayConfig, log: MessageLog, logger: PinoLogger): void {
  const cutoff = Date.now() - cfg.messageRetentionMs;
  const rows = log.deleteOlderThan(cutoff);
  const dirs: string[] = [];
  if (existsSync(cfg.consoleFilesDir)) {
    for (const name of readdirSync(cfg.consoleFilesDir)) {
      const end = endOfLocalDay(name);
      if (end !== null && end <= cutoff) {
        rmSync(join(cfg.consoleFilesDir, name), { recursive: true, force: true });
        dirs.push(name);
      }
    }
  }
  if (rows > 0 || dirs.length > 0) {
    logger.info({ rows, consoleFileDirs: dirs }, "retention purge");
  }
}

async function main(): Promise<void> {
  const cfg = loadConfigFromArgs(process.argv.slice(2));
  const logger = createLogger(cfg.logLevel);
  const chatLogger = toChatLogger(logger);
  logger.info(
    { platforms: Object.keys(cfg.platforms), console: cfg.console !== undefined, pid: process.pid },
    "gateway starting",
  );

  const db: Db = openDb(cfg.database);
  const log = new MessageLog(db);
  finishInterruptedDownloads(log, logger);

  const platforms = new Platforms(cfg, logger, chatLogger);
  const repo = new Repo(db, {
    consoleEnabled: cfg.console !== undefined,
    classifierEnabledDefault: cfg.classifierEnabledDefault,
    platforms: platforms.names(),
  });
  if (cfg.console) {
    repo.ensureConsoleBinding();
  }
  const tokens = new Tokens(db);
  const agentState = new AgentState(repo);
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

  agentState.onChange(({ previous, state, reason }) => {
    logger.info({ previous, state, reason }, "agent state");
    platforms.console?.printStatus(`agent：${previous !== null ? `${previous} → ` : ""}${state}${reason ? `（${reason}）` : ""}`);
    if (state === "running") {
      bridge.pump();
    }
  });

  const state = createSqliteState(db);
  const bot = new Chat({
    userName: cfg.userName,
    adapters: platforms.adapters,
    state,
    concurrency: "concurrent",
    logger: chatLogger,
  });
  bot.onDirectMessage((thread, message) => ingest.message(thread, message));
  bot.onNewMention((thread, message) => ingest.message(thread, message));
  bot.onNewMessage(/[\s\S]*/, (thread, message) => ingest.message(thread, message));
  bot.onSlashCommand((event) => ingest.command(event));
  bot.onAction((event) => ingest.action(event));
  bot.onReaction((event) => ingest.reaction(event));
  await bot.initialize();

  await ctl.listen();
  await bridge.listen();
  const webhookPlatforms = new Set([...platforms.names()].filter((name) => name !== "console"));
  const http = cfg.http.listen !== "" ? new HttpServer(cfg.http.listen, bot, webhookPlatforms, logger) : null;
  await http?.start();

  purgeRetention(cfg, log, logger);
  setInterval(() => purgeRetention(cfg, log, logger), RETENTION_INTERVAL_MS).unref();

  let shuttingDown = false;
  const shutdown = async (signal: string): Promise<void> => {
    if (shuttingDown) {
      return;
    }
    shuttingDown = true;
    logger.info({ signal }, "shutting down");
    await platforms.stopReceiving();
    await outbound.whenIdle();
    await http?.close();
    await bridge.close();
    await ctl.close();
    await bot.shutdown();
    db.$client.close();
    logger.info("gateway stopped");
    process.exit(0);
  };
  process.on("SIGTERM", () => void shutdown("SIGTERM"));
  process.on("SIGINT", () => void shutdown("SIGINT"));
  logger.info("gateway ready");
}

await main();
