import { randomBytes } from "node:crypto";
import type Database from "better-sqlite3";
import type { Lock, QueueEntry, StateAdapter } from "chat";
import type { Db } from "../db/index.ts";

const PURGE_INTERVAL_MS = 10 * 60 * 1000;

type Entry = { kind: "kv"; value: unknown } | { kind: "list"; values: unknown[] };

function encode(value: unknown): string {
  const json = JSON.stringify(value);
  if (json === undefined) {
    throw new TypeError(`state value of type ${typeof value} cannot be stored as JSON`);
  }
  return json;
}

function expiry(now: number, ttlMs: number | undefined): number | null {
  return ttlMs ? now + ttlMs : null;
}

function generateToken(): string {
  return `sqlite_${Date.now()}_${randomBytes(8).toString("hex")}`;
}

function prepareStatements(s: Database.Database) {
  return {
    subscribe: s.prepare("INSERT OR IGNORE INTO sdk_subscriptions (thread_id) VALUES (?)"),
    unsubscribe: s.prepare("DELETE FROM sdk_subscriptions WHERE thread_id = ?"),
    isSubscribed: s.prepare<[string], number>("SELECT 1 FROM sdk_subscriptions WHERE thread_id = ?").pluck(),

    lockDeleteExpired: s.prepare("DELETE FROM sdk_locks WHERE expires_at <= ?"),
    lockGet: s.prepare<[string], { token: string; expires_at: number }>(
      "SELECT token, expires_at FROM sdk_locks WHERE thread_id = ?",
    ),
    lockInsert: s.prepare("INSERT INTO sdk_locks (thread_id, token, expires_at) VALUES (?, ?, ?)"),
    lockDelete: s.prepare("DELETE FROM sdk_locks WHERE thread_id = ?"),
    lockDeleteToken: s.prepare("DELETE FROM sdk_locks WHERE thread_id = ? AND token = ?"),
    lockExtend: s.prepare("UPDATE sdk_locks SET expires_at = ? WHERE thread_id = ?"),

    kvGet: s.prepare<[string], { value: string; expires_at: number | null }>(
      "SELECT value, expires_at FROM sdk_kv WHERE key = ?",
    ),
    kvPut: s.prepare("INSERT OR REPLACE INTO sdk_kv (key, value, expires_at) VALUES (?, ?, ?)"),
    kvDelete: s.prepare("DELETE FROM sdk_kv WHERE key = ?"),

    listGet: s.prepare<[string], { value: string; expires_at: number | null }>(
      "SELECT value, expires_at FROM sdk_lists WHERE key = ? ORDER BY idx",
    ),
    listMaxIdx: s.prepare<[string], number | null>("SELECT MAX(idx) FROM sdk_lists WHERE key = ?").pluck(),
    listCount: s.prepare<[string], number>("SELECT COUNT(*) FROM sdk_lists WHERE key = ?").pluck(),
    listInsert: s.prepare("INSERT INTO sdk_lists (key, idx, value, expires_at) VALUES (?, ?, ?, ?)"),
    listTrim: s.prepare(
      `DELETE FROM sdk_lists WHERE key = ? AND idx IN
         (SELECT idx FROM sdk_lists WHERE key = ? ORDER BY idx LIMIT ?)`,
    ),
    listTouch: s.prepare("UPDATE sdk_lists SET expires_at = ? WHERE key = ?"),
    listDelete: s.prepare("DELETE FROM sdk_lists WHERE key = ?"),

    queueMaxIdx: s.prepare<[string], number | null>("SELECT MAX(idx) FROM sdk_queues WHERE thread_id = ?").pluck(),
    queueCount: s.prepare<[string], number>("SELECT COUNT(*) FROM sdk_queues WHERE thread_id = ?").pluck(),
    queueInsert: s.prepare("INSERT INTO sdk_queues (thread_id, idx, entry, expires_at) VALUES (?, ?, ?, ?)"),
    queueTrim: s.prepare(
      `DELETE FROM sdk_queues WHERE thread_id = ? AND idx IN
         (SELECT idx FROM sdk_queues WHERE thread_id = ? ORDER BY idx LIMIT ?)`,
    ),
    queueFirst: s.prepare<[string], { idx: number; entry: string }>(
      "SELECT idx, entry FROM sdk_queues WHERE thread_id = ? ORDER BY idx LIMIT 1",
    ),
    queueDelete: s.prepare("DELETE FROM sdk_queues WHERE thread_id = ? AND idx = ?"),

    purgeKv: s.prepare("DELETE FROM sdk_kv WHERE expires_at IS NOT NULL AND expires_at <= ?"),
    purgeLists: s.prepare("DELETE FROM sdk_lists WHERE expires_at IS NOT NULL AND expires_at <= ?"),
    purgeLocks: s.prepare("DELETE FROM sdk_locks WHERE expires_at <= ?"),
    purgeQueues: s.prepare("DELETE FROM sdk_queues WHERE expires_at <= ?"),
  };
}

/**
 * chat-sdk StateAdapter on the gateway database (DESIGN 6.3).
 *
 * Semantics follow @chat-adapter/state-memory: keys and lists share one key space, expired rows count as
 * absent at read time, every list append resets the TTL of the whole list. Unlike the memory adapter,
 * disconnect() keeps all data, because the rows live in the database.
 */
export class SqliteStateAdapter implements StateAdapter {
  private readonly sqlite: Database.Database;
  private connected = false;
  private purgeTimer: NodeJS.Timeout | null = null;

  private readonly q: ReturnType<typeof prepareStatements>;

  constructor(db: Db, private readonly purgeIntervalMs: number = PURGE_INTERVAL_MS) {
    this.sqlite = db.$client;
    this.q = prepareStatements(this.sqlite);
  }

  async connect(): Promise<void> {
    if (this.connected) {
      return;
    }
    this.connected = true;
    this.purgeTimer = setInterval(() => this.purgeExpired(), this.purgeIntervalMs);
    this.purgeTimer.unref();
  }

  async disconnect(): Promise<void> {
    this.connected = false;
    if (this.purgeTimer) {
      clearInterval(this.purgeTimer);
      this.purgeTimer = null;
    }
  }

  async subscribe(threadId: string): Promise<void> {
    this.ensureConnected();
    this.q.subscribe.run(threadId);
  }

  async unsubscribe(threadId: string): Promise<void> {
    this.ensureConnected();
    this.q.unsubscribe.run(threadId);
  }

  async isSubscribed(threadId: string): Promise<boolean> {
    this.ensureConnected();
    return this.q.isSubscribed.get(threadId) !== undefined;
  }

  async acquireLock(threadId: string, ttlMs: number): Promise<Lock | null> {
    this.ensureConnected();
    return this.sqlite
      .transaction((): Lock | null => {
        const now = Date.now();
        this.q.lockDeleteExpired.run(now);
        if (this.q.lockGet.get(threadId)) {
          return null;
        }
        const lock: Lock = { threadId, token: generateToken(), expiresAt: now + ttlMs };
        this.q.lockInsert.run(threadId, lock.token, lock.expiresAt);
        return lock;
      })
      .immediate();
  }

  async forceReleaseLock(threadId: string): Promise<void> {
    this.ensureConnected();
    this.q.lockDelete.run(threadId);
  }

  async releaseLock(lock: Lock): Promise<void> {
    this.ensureConnected();
    this.q.lockDeleteToken.run(lock.threadId, lock.token);
  }

  async extendLock(lock: Lock, ttlMs: number): Promise<boolean> {
    this.ensureConnected();
    return this.sqlite
      .transaction((): boolean => {
        const now = Date.now();
        const row = this.q.lockGet.get(lock.threadId);
        if (!row || row.token !== lock.token) {
          return false;
        }
        if (row.expires_at < now) {
          this.q.lockDelete.run(lock.threadId);
          return false;
        }
        this.q.lockExtend.run(now + ttlMs, lock.threadId);
        return true;
      })
      .immediate();
  }

  async get<T = unknown>(key: string): Promise<T | null> {
    this.ensureConnected();
    const entry = this.sqlite.transaction(() => this.readEntry(key, Date.now())).immediate();
    if (!entry) {
      return null;
    }
    return (entry.kind === "kv" ? entry.value : entry.values) as T;
  }

  async set<T = unknown>(key: string, value: T, ttlMs?: number): Promise<void> {
    this.ensureConnected();
    const json = encode(value);
    this.sqlite
      .transaction(() => {
        this.q.listDelete.run(key);
        this.q.kvPut.run(key, json, expiry(Date.now(), ttlMs));
      })
      .immediate();
  }

  async setIfNotExists(key: string, value: unknown, ttlMs?: number): Promise<boolean> {
    this.ensureConnected();
    const json = encode(value);
    return this.sqlite
      .transaction((): boolean => {
        const now = Date.now();
        if (this.readEntry(key, now)) {
          return false;
        }
        this.q.kvPut.run(key, json, expiry(now, ttlMs));
        return true;
      })
      .immediate();
  }

  async delete(key: string): Promise<void> {
    this.ensureConnected();
    this.sqlite
      .transaction(() => {
        this.q.kvDelete.run(key);
        this.q.listDelete.run(key);
      })
      .immediate();
  }

  async appendToList(key: string, value: unknown, options?: { maxLength?: number; ttlMs?: number }): Promise<void> {
    this.ensureConnected();
    const json = encode(value);
    this.sqlite
      .transaction(() => {
        const now = Date.now();
        const expiresAt = expiry(now, options?.ttlMs);
        const entry = this.readEntry(key, now);
        if (entry?.kind === "kv") {
          // A plain value under this key: an array value becomes the start of the list, anything else is replaced.
          this.q.kvDelete.run(key);
          const base = Array.isArray(entry.value) ? (entry.value as unknown[]) : [];
          base.forEach((item, i) => this.q.listInsert.run(key, i, encode(item), expiresAt));
        }
        const max = this.q.listMaxIdx.get(key);
        this.q.listInsert.run(key, max === null || max === undefined ? 0 : max + 1, json, expiresAt);
        const maxLength = options?.maxLength;
        if (maxLength) {
          const count = this.q.listCount.get(key) ?? 0;
          if (count > maxLength) {
            this.q.listTrim.run(key, key, count - maxLength);
          }
        }
        this.q.listTouch.run(expiresAt, key);
      })
      .immediate();
  }

  async getList<T = unknown>(key: string): Promise<T[]> {
    this.ensureConnected();
    const entry = this.sqlite.transaction(() => this.readEntry(key, Date.now())).immediate();
    if (!entry) {
      return [];
    }
    if (entry.kind === "list") {
      return entry.values as T[];
    }
    return Array.isArray(entry.value) ? (entry.value as T[]) : [];
  }

  async enqueue(threadId: string, entry: QueueEntry, maxSize: number): Promise<number> {
    this.ensureConnected();
    const json = encode(entry);
    return this.sqlite
      .transaction((): number => {
        const max = this.q.queueMaxIdx.get(threadId);
        this.q.queueInsert.run(threadId, max === null || max === undefined ? 0 : max + 1, json, entry.expiresAt);
        const count = this.q.queueCount.get(threadId) ?? 0;
        if (count > maxSize) {
          this.q.queueTrim.run(threadId, threadId, count - maxSize);
          return maxSize;
        }
        return count;
      })
      .immediate();
  }

  async dequeue(threadId: string): Promise<QueueEntry | null> {
    this.ensureConnected();
    return this.sqlite
      .transaction((): QueueEntry | null => {
        const row = this.q.queueFirst.get(threadId);
        if (!row) {
          return null;
        }
        this.q.queueDelete.run(threadId, row.idx);
        return JSON.parse(row.entry) as QueueEntry;
      })
      .immediate();
  }

  async queueDepth(threadId: string): Promise<number> {
    this.ensureConnected();
    return this.q.queueCount.get(threadId) ?? 0;
  }

  /** Deletes expired rows of all state tables. Runs every 10 minutes while connected. */
  purgeExpired(now: number = Date.now()): { kv: number; lists: number; locks: number; queues: number } {
    return this.sqlite
      .transaction(() => ({
        kv: this.q.purgeKv.run(now).changes,
        lists: this.q.purgeLists.run(now).changes,
        locks: this.q.purgeLocks.run(now).changes,
        queues: this.q.purgeQueues.run(now).changes,
      }))
      .immediate();
  }

  /** Reads the value or list under `key`; expired rows are deleted and count as absent. */
  private readEntry(key: string, now: number): Entry | null {
    const kv = this.q.kvGet.get(key);
    if (kv) {
      if (kv.expires_at !== null && kv.expires_at <= now) {
        this.q.kvDelete.run(key);
        return null;
      }
      return { kind: "kv", value: JSON.parse(kv.value) };
    }
    const rows = this.q.listGet.all(key);
    if (rows.length === 0) {
      return null;
    }
    const expiresAt = rows[0]!.expires_at;
    if (expiresAt !== null && expiresAt <= now) {
      this.q.listDelete.run(key);
      return null;
    }
    return { kind: "list", values: rows.map((r) => JSON.parse(r.value) as unknown) };
  }

  private ensureConnected(): void {
    if (!this.connected) {
      throw new Error("SqliteStateAdapter is not connected. Call connect() first.");
    }
  }
}

export function createSqliteState(db: Db, purgeIntervalMs?: number): SqliteStateAdapter {
  return new SqliteStateAdapter(db, purgeIntervalMs);
}
