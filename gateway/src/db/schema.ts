import { sql } from "drizzle-orm";
import { check, index, integer, primaryKey, sqliteTable, text } from "drizzle-orm/sqlite-core";
import type { InboundPayload, OutboundPayload } from "../types.ts";

// All times are Unix milliseconds.

export const admin = sqliteTable("admin", {
  identity: text("identity").primaryKey(),
  displayName: text("display_name"),
  notifyThread: text("notify_thread"),
  addedBy: text("added_by"),
  addedAt: integer("added_at").notNull(),
});

export const adminToken = sqliteTable("admin_token", {
  tokenHash: text("token_hash").primaryKey(),
  createdAt: integer("created_at").notNull(),
  expiresAt: integer("expires_at").notNull(),
  usedBy: text("used_by"),
  usedAt: integer("used_at"),
});

export const binding = sqliteTable(
  "binding",
  {
    channelId: text("channel_id").primaryKey(),
    platform: text("platform").notNull(),
    channelName: text("channel_name"),
    trigger: text("trigger", { enum: ["mention", "all", "prefix"] }).notNull(),
    prefix: text("prefix"),
    replyMode: text("reply_mode", { enum: ["final", "each"] }).notNull(),
    interrupt: text("interrupt", { enum: ["queue", "steer"] }).notNull(),
    createdBy: text("created_by").notNull(),
    createdAt: integer("created_at").notNull(),
  },
  (t) => [
    check("binding_trigger", sql`${t.trigger} in ('mention', 'all', 'prefix')`),
    check("binding_reply_mode", sql`${t.replyMode} in ('final', 'each')`),
    check("binding_interrupt", sql`${t.interrupt} in ('queue', 'steer')`),
    check("binding_prefix", sql`${t.trigger} <> 'prefix' or (${t.prefix} is not null and ${t.prefix} <> '')`),
  ],
);

export const messageLog = sqliteTable(
  "message_log",
  {
    // AUTOINCREMENT keeps seq monotonic even after the retention purge removed every row.
    seq: integer("seq").primaryKey({ autoIncrement: true }),
    direction: text("direction", { enum: ["in", "out"] }).notNull(),
    kind: text("kind", { enum: ["message", "command", "action", "reaction"] }).notNull(),
    threadId: text("thread_id").notNull(),
    channelId: text("channel_id").notNull(),
    messageId: text("message_id").notNull(),
    payload: text("payload", { mode: "json" }).$type<InboundPayload | OutboundPayload>().notNull(),
    createdAt: integer("created_at").notNull(),
  },
  (t) => [
    check("message_log_direction", sql`${t.direction} in ('in', 'out')`),
    check("message_log_kind", sql`${t.kind} in ('message', 'command', 'action', 'reaction')`),
    index("message_log_created_at").on(t.createdAt),
    index("message_log_channel").on(t.channelId, t.seq),
    index("message_log_thread").on(t.threadId, t.seq),
  ],
);

export const bridgeCursor = sqliteTable(
  "bridge_cursor",
  {
    id: integer("id").primaryKey(),
    ackedSeq: integer("acked_seq").notNull(),
  },
  (t) => [check("bridge_cursor_single_row", sql`${t.id} = 1`)],
);

export const classifierSetting = sqliteTable(
  "classifier_setting",
  {
    id: integer("id").primaryKey(),
    enabled: integer("enabled", { mode: "boolean" }).notNull(),
    updatedBy: text("updated_by").notNull(),
    updatedAt: integer("updated_at").notNull(),
  },
  (t) => [check("classifier_setting_single_row", sql`${t.id} = 1`)],
);

export const classifierRule = sqliteTable(
  "classifier_rule",
  {
    pattern: text("pattern").primaryKey(),
    action: text("action", { enum: ["allow", "deny"] }).notNull(),
    createdBy: text("created_by").notNull(),
    createdAt: integer("created_at").notNull(),
  },
  (t) => [check("classifier_rule_action", sql`${t.action} in ('allow', 'deny')`)],
);

/** Channels that already got the maintenance notice in the current maintenance period (DESIGN 5.8 step 4). */
export const maintenanceNotice = sqliteTable("maintenance_notice", {
  channelId: text("channel_id").primaryKey(),
});

// chat-sdk StateAdapter tables (DESIGN 6.3). Values are JSON text; expires_at null = no expiry.

export const sdkKv = sqliteTable(
  "sdk_kv",
  {
    key: text("key").primaryKey(),
    value: text("value").notNull(),
    expiresAt: integer("expires_at"),
  },
  (t) => [index("sdk_kv_expires_at").on(t.expiresAt)],
);

export const sdkLists = sqliteTable(
  "sdk_lists",
  {
    key: text("key").notNull(),
    idx: integer("idx").notNull(),
    value: text("value").notNull(),
    expiresAt: integer("expires_at"),
  },
  (t) => [primaryKey({ columns: [t.key, t.idx] }), index("sdk_lists_expires_at").on(t.expiresAt)],
);

export const sdkLocks = sqliteTable("sdk_locks", {
  threadId: text("thread_id").primaryKey(),
  token: text("token").notNull(),
  expiresAt: integer("expires_at").notNull(),
});

export const sdkQueues = sqliteTable(
  "sdk_queues",
  {
    threadId: text("thread_id").notNull(),
    idx: integer("idx").notNull(),
    entry: text("entry").notNull(),
    expiresAt: integer("expires_at").notNull(),
  },
  (t) => [primaryKey({ columns: [t.threadId, t.idx] }), index("sdk_queues_expires_at").on(t.expiresAt)],
);

export const sdkSubscriptions = sqliteTable("sdk_subscriptions", {
  threadId: text("thread_id").primaryKey(),
});
