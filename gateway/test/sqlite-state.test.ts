// DESIGN 16 "SQLite state": the same operation sequences against createSqliteState and
// @chat-adapter/state-memory must give the same results.
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { createMemoryState } from "@chat-adapter/state-memory";
import { Message, type Lock, type QueueEntry, type StateAdapter } from "chat";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { openDb, type Db } from "../src/db/index.ts";
import { SqliteStateAdapter, createSqliteState } from "../src/state/sqlite-state.ts";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

let dir: string;
let db: Db;

beforeEach(() => {
  dir = mkdtempSync("/tmp/bbstate");
  db = openDb(join(dir, "gateway.db"));
});

afterEach(() => {
  db.$client.close();
  rmSync(dir, { recursive: true, force: true });
});

type Op = (s: StateAdapter) => Promise<unknown>;

/** Runs the steps on both adapters in lock step (same wall clock) and returns both result lists. */
async function compare(steps: (Op | { sleep: number })[]): Promise<{ memory: unknown[]; sqlite: unknown[] }> {
  const memory = createMemoryState();
  const sqlite = createSqliteState(db);
  await memory.connect();
  await sqlite.connect();
  const results = { memory: [] as unknown[], sqlite: [] as unknown[] };
  for (const step of steps) {
    if ("sleep" in step) {
      await sleep(step.sleep);
      continue;
    }
    results.memory.push(normalize(await step(memory)));
    results.sqlite.push(normalize(await step(sqlite)));
  }
  await memory.disconnect();
  await sqlite.disconnect();
  return results;
}

/** Lock tokens and times differ between implementations; compare their meaning. */
function normalize(v: unknown): unknown {
  if (v && typeof v === "object" && "token" in v && "expiresAt" in v && "threadId" in v) {
    const lock = v as Lock;
    return { lock: lock.threadId, ttlLeft: Math.round((lock.expiresAt - Date.now()) / 1000) };
  }
  return JSON.parse(JSON.stringify(v ?? null));
}

describe("SqliteStateAdapter matches MemoryStateAdapter", () => {
  test("subscriptions", async () => {
    const r = await compare([
      (s) => s.isSubscribed("t1"),
      (s) => s.subscribe("t1"),
      (s) => s.subscribe("t1"),
      (s) => s.subscribe("t2"),
      (s) => s.isSubscribed("t1"),
      (s) => s.isSubscribed("t2"),
      (s) => s.unsubscribe("t1"),
      (s) => s.isSubscribed("t1"),
      (s) => s.unsubscribe("never"),
      (s) => s.isSubscribed("t2"),
    ]);
    expect(r.sqlite).toEqual(r.memory);
    expect(r.memory).toEqual([false, null, null, null, true, true, null, false, null, true]);
  });

  test("locks: acquire, conflict, extend, release by token, force release, expiry", async () => {
    // Locks returned by each implementation, by name, for the later extend/release calls.
    const locks = new Map<StateAdapter, Map<string, Lock | null>>();
    const keep = (name: string, s: StateAdapter, l: Lock | null) => {
      if (!locks.has(s)) locks.set(s, new Map());
      locks.get(s)!.set(name, l);
      return l;
    };
    const get = (name: string, s: StateAdapter) => locks.get(s)!.get(name)!;
    const r = await compare([
      async (s) => keep("a", s, await s.acquireLock("k", 5000)),
      (s) => s.acquireLock("k", 5000),
      (s) => s.extendLock({ ...get("a", s), token: "wrong" }, 5000),
      (s) => s.extendLock(get("a", s), 10_000),
      (s) => s.releaseLock({ ...get("a", s), token: "wrong" }),
      (s) => s.acquireLock("k", 5000),
      (s) => s.releaseLock(get("a", s)),
      async (s) => keep("b", s, await s.acquireLock("k", 5000)),
      (s) => s.forceReleaseLock("k"),
      async (s) => keep("c", s, await s.acquireLock("k", 40)),
      { sleep: 80 },
      (s) => s.extendLock(get("c", s), 5000),
      async (s) => keep("d", s, await s.acquireLock("k", 40)),
      { sleep: 80 },
      async (s) => keep("e", s, await s.acquireLock("k", 5000)),
      (s) => s.extendLock(get("d", s), 5000),
      (s) => s.acquireLock("chat:callback:abc", 10_000),
      (s) => s.acquireLock("other", 5000),
    ]);
    expect(r.sqlite).toEqual(r.memory);
    expect(r.memory).toEqual([
      { lock: "k", ttlLeft: 5 },
      null,
      false,
      true,
      null,
      null,
      null,
      { lock: "k", ttlLeft: 5 },
      null,
      { lock: "k", ttlLeft: 0 },
      false,
      { lock: "k", ttlLeft: 0 },
      { lock: "k", ttlLeft: 5 },
      false,
      { lock: "chat:callback:abc", ttlLeft: 10 },
      { lock: "other", ttlLeft: 5 },
    ]);
  });

  test("key/value with TTL and setIfNotExists", async () => {
    const r = await compare([
      (s) => s.get("missing"),
      (s) => s.set("n", 1),
      (s) => s.get("n"),
      (s) => s.set("obj", { a: [1, 2, { b: "x" }], c: null, d: "文字" }),
      (s) => s.get("obj"),
      (s) => s.set("n", "replaced"),
      (s) => s.get("n"),
      (s) => s.set("short", true, 40),
      (s) => s.get("short"),
      { sleep: 80 },
      (s) => s.get("short"),
      (s) => s.setIfNotExists("dedupe:console:1", true, 40),
      (s) => s.setIfNotExists("dedupe:console:1", true, 40),
      (s) => s.get("dedupe:console:1"),
      { sleep: 80 },
      (s) => s.setIfNotExists("dedupe:console:1", "again", 40),
      (s) => s.get("dedupe:console:1"),
      (s) => s.setIfNotExists("forever", 1),
      (s) => s.set("zero", 5, 0),
      { sleep: 30 },
      (s) => s.get("zero"),
      (s) => s.get("forever"),
      (s) => s.delete("n"),
      (s) => s.get("n"),
      (s) => s.delete("never-existed"),
    ]);
    expect(r.sqlite).toEqual(r.memory);
    expect(r.memory).toEqual([
      null, null, 1, null, { a: [1, 2, { b: "x" }], c: null, d: "文字" }, null, "replaced", null, true, null,
      true, false, true, true, "again", true, null, 5, 1, null, null, null,
    ]);
  });

  test("lists: maxLength, TTL reset on append, expiry restarts the list", async () => {
    const r = await compare([
      (s) => s.getList("missing"),
      (s) => s.appendToList("l", 1, { maxLength: 3 }),
      (s) => s.appendToList("l", 2, { maxLength: 3 }),
      (s) => s.appendToList("l", 3, { maxLength: 3 }),
      (s) => s.appendToList("l", 4, { maxLength: 3 }),
      (s) => s.appendToList("l", { five: 5 }, { maxLength: 3 }),
      (s) => s.getList("l"),
      (s) => s.appendToList("t", "a", { ttlMs: 400 }),
      { sleep: 250 },
      (s) => s.appendToList("t", "b", { ttlMs: 400 }),
      { sleep: 250 },
      (s) => s.getList("t"),
      { sleep: 250 },
      (s) => s.getList("t"),
      (s) => s.appendToList("t", "c", { ttlMs: 400 }),
      (s) => s.getList("t"),
      (s) => s.appendToList("p", "x", { ttlMs: 40 }),
      (s) => s.appendToList("p", "y"),
      { sleep: 80 },
      (s) => s.getList("p"),
      (s) => s.appendToList("nolimit", 1, { maxLength: 0 }),
      (s) => s.appendToList("nolimit", 2, { maxLength: 0 }),
      (s) => s.getList("nolimit"),
      (s) => s.delete("l"),
      (s) => s.getList("l"),
    ]);
    expect(r.sqlite).toEqual(r.memory);
    expect(r.memory).toEqual([
      [], null, null, null, null, null, [3, 4, { five: 5 }], null, null, ["a", "b"], [], null, ["c"], null, null,
      ["x", "y"], null, null, [1, 2], null, [],
    ]);
  });

  test("keys and lists share one key space", async () => {
    const r = await compare([
      (s) => s.appendToList("shared", 1),
      (s) => s.get("shared"),
      (s) => s.setIfNotExists("shared", "v"),
      (s) => s.set("shared", "plain"),
      (s) => s.getList("shared"),
      (s) => s.appendToList("shared", 2),
      (s) => s.getList("shared"),
      (s) => s.set("arr", [7, 8]),
      (s) => s.getList("arr"),
      (s) => s.appendToList("arr", 9),
      (s) => s.getList("arr"),
      (s) => s.get("arr"),
    ]);
    expect(r.sqlite).toEqual(r.memory);
    expect(r.memory).toEqual([null, [1], false, null, [], null, [2], null, [7, 8], null, [7, 8, 9], [7, 8, 9]]);
  });

  test("queues: FIFO, maxSize drops the oldest, depth", async () => {
    const entry = (n: number): QueueEntry => ({
      enqueuedAt: 1_700_000_000_000 + n,
      expiresAt: Date.now() + 90_000,
      message: new Message({
        id: `m${n}`,
        threadId: "console:main",
        text: `text ${n}`,
        formatted: { type: "root", children: [] },
        raw: null,
        author: { userId: "operator", userName: "operator", fullName: "控制台", isBot: false, isMe: false },
        metadata: { dateSent: new Date(1_700_000_000_000 + n), edited: false },
        attachments: [],
      }),
    });
    const entries = [1, 2, 3, 4].map(entry);
    const r = await compare([
      (s) => s.dequeue("q"),
      (s) => s.queueDepth("q"),
      (s) => s.enqueue("q", entries[0]!, 3),
      (s) => s.enqueue("q", entries[1]!, 3),
      (s) => s.enqueue("q", entries[2]!, 3),
      (s) => s.enqueue("q", entries[3]!, 3),
      (s) => s.queueDepth("q"),
      (s) => s.dequeue("q"),
      (s) => s.queueDepth("q"),
      (s) => s.dequeue("q"),
      (s) => s.dequeue("q"),
      (s) => s.dequeue("q"),
      (s) => s.queueDepth("q"),
      (s) => s.enqueue("z", entries[0]!, 0),
      (s) => s.queueDepth("z"),
    ]);
    expect(r.sqlite).toEqual(r.memory);
    const ids = (r.memory as any[]).map((v) => (v && typeof v === "object" ? v.message.id : v));
    expect(ids).toEqual([null, 0, 1, 2, 3, 3, 3, "m2", 2, "m3", "m4", null, 0, 0, 0]);
  });

  test("every method fails before connect()", async () => {
    const memory = createMemoryState();
    const sqlite = createSqliteState(db);
    const calls: Op[] = [
      (s) => s.get("k"),
      (s) => s.set("k", 1),
      (s) => s.setIfNotExists("k", 1),
      (s) => s.delete("k"),
      (s) => s.appendToList("k", 1),
      (s) => s.getList("k"),
      (s) => s.subscribe("t"),
      (s) => s.isSubscribed("t"),
      (s) => s.acquireLock("t", 1),
      (s) => s.queueDepth("t"),
    ];
    for (const call of calls) {
      await expect(call(memory)).rejects.toThrow(/not connected/);
      await expect(call(sqlite)).rejects.toThrow(/not connected/);
    }
  });
});

describe("SqliteStateAdapter persistence and maintenance", () => {
  test("data survives disconnect and a new adapter on the same database", async () => {
    const a = createSqliteState(db);
    await a.connect();
    await a.subscribe("telegram:1");
    await a.set("telegram:polling:x", { offset: 42 });
    await a.appendToList("msg-history:console:main", { id: "1" }, { maxLength: 100, ttlMs: 60_000 });
    await a.disconnect();

    db.$client.close();
    db = openDb(join(dir, "gateway.db"));
    const b = createSqliteState(db);
    await b.connect();
    expect(await b.isSubscribed("telegram:1")).toBe(true);
    expect(await b.get("telegram:polling:x")).toEqual({ offset: 42 });
    expect(await b.getList("msg-history:console:main")).toEqual([{ id: "1" }]);
    await b.disconnect();
  });

  test("purgeExpired deletes expired rows of every table and keeps the rest", async () => {
    const s = createSqliteState(db) as SqliteStateAdapter;
    await s.connect();
    await s.set("old", 1, 10);
    await s.set("keep", 2);
    await s.appendToList("oldlist", 1, { ttlMs: 10 });
    await s.appendToList("keeplist", 1, { ttlMs: 60_000 });
    await s.acquireLock("oldlock", 10);
    await s.enqueue("q", { enqueuedAt: Date.now(), expiresAt: Date.now() + 10, message: {} as Message }, 5);
    await s.enqueue("q", { enqueuedAt: Date.now(), expiresAt: Date.now() + 60_000, message: {} as Message }, 5);
    await sleep(30);
    expect(s.purgeExpired()).toEqual({ kv: 1, lists: 1, locks: 1, queues: 1 });
    const count = (table: string) => db.$client.prepare(`select count(*) from ${table}`).pluck().get();
    expect(count("sdk_kv")).toBe(1);
    expect(count("sdk_lists")).toBe(1);
    expect(count("sdk_locks")).toBe(0);
    expect(count("sdk_queues")).toBe(1);
    expect(await s.get("keep")).toBe(2);
    await s.disconnect();
  });

  test("setIfNotExists is atomic: exactly one of many concurrent calls wins", async () => {
    const s = createSqliteState(db);
    await s.connect();
    const results = await Promise.all(Array.from({ length: 50 }, (_, i) => s.setIfNotExists("dedupe:x", i, 60_000)));
    expect(results.filter(Boolean)).toHaveLength(1);
    await s.disconnect();
  });

  test("values that are not JSON are rejected", async () => {
    const s = createSqliteState(db);
    await s.connect();
    await expect(s.set("u", undefined)).rejects.toThrow(/cannot be stored as JSON/);
    await s.disconnect();
  });
});
