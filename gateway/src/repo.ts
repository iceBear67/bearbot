import { asc, eq, inArray, lte } from "drizzle-orm";
import type { Db } from "./db/index.ts";
import { admin, binding, bridgeCursor, bridgeDone, classifierRule, classifierSetting, maintenanceNotice } from "./db/schema.ts";
import { isoLocal } from "./time.ts";
import type { AdminInfo, ClassifierInfo, Interrupt, ReplyMode, Trigger, WireBinding } from "./types.ts";

export const CONSOLE_PLATFORM = "console";
export const CONSOLE_CHANNEL = "console:main";
export const CONSOLE_OPERATOR = "console:operator";
export const CONSOLE_CHANNEL_NAME = "控制台";

export type AdminRow = typeof admin.$inferSelect;
export type BindingRow = typeof binding.$inferSelect;
export type ClassifierSettingRow = typeof classifierSetting.$inferSelect;

/** Platform part of a thread id, channel id or identity ("telegram:-100" -> "telegram"). */
export function platformOf(id: string): string {
  const i = id.indexOf(":");
  return i < 0 ? id : id.slice(0, i);
}

/** Admins, bindings, classifier settings, maintenance notices and the bridge cursor (DESIGN 6.4, 5.8). */
export class Repo {
  constructor(
    private readonly db: Db,
    private readonly options: {
      consoleEnabled: boolean;
      classifierEnabledDefault: boolean;
      /** Names of adapters registered in the Chat object. */
      platforms: ReadonlySet<string>;
    },
  ) {}

  get consoleEnabled(): boolean {
    return this.options.consoleEnabled;
  }

  // ---- admins ----

  adminRows(): AdminRow[] {
    return this.db.select().from(admin).orderBy(asc(admin.addedAt), asc(admin.identity)).all();
  }

  adminRow(identity: string): AdminRow | undefined {
    return this.db.select().from(admin).where(eq(admin.identity, identity)).get();
  }

  isBuiltinAdmin(identity: string): boolean {
    return this.options.consoleEnabled && identity === CONSOLE_OPERATOR;
  }

  isAdmin(identity: string): boolean {
    return this.isBuiltinAdmin(identity) || this.adminRow(identity) !== undefined;
  }

  /** Table admins in order of addition, then the builtin console admin (DESIGN 6.2.4). */
  adminIdentities(): string[] {
    const ids = this.adminRows().map((r) => r.identity);
    if (this.options.consoleEnabled) {
      ids.push(CONSOLE_OPERATOR);
    }
    return ids;
  }

  adminInfos(): AdminInfo[] {
    const infos: AdminInfo[] = this.adminRows().map((r) => ({
      identity: r.identity,
      displayName: r.displayName,
      notifyThread: r.notifyThread,
      addedBy: r.addedBy,
      addedAt: isoLocal(new Date(r.addedAt)),
      builtin: false,
    }));
    if (this.options.consoleEnabled) {
      infos.push({
        identity: CONSOLE_OPERATOR,
        displayName: CONSOLE_CHANNEL_NAME,
        notifyThread: CONSOLE_CHANNEL,
        addedBy: null,
        addedAt: null,
        builtin: true,
      });
    }
    return infos;
  }

  /** notify_thread of every admin in admin order, each distinct thread once. */
  notifyThreads(): string[] {
    const seen = new Set<string>();
    for (const info of this.adminInfos()) {
      if (info.notifyThread !== null) {
        seen.add(info.notifyThread);
      }
    }
    return [...seen];
  }

  /** Inserts the admin, or updates display name and notify thread of an existing one. */
  putAdmin(row: { identity: string; displayName: string | null; notifyThread: string | null; addedBy: string }): void {
    this.db
      .insert(admin)
      .values({ ...row, addedAt: Date.now() })
      .onConflictDoUpdate({
        target: admin.identity,
        set: { displayName: row.displayName, notifyThread: row.notifyThread },
      })
      .run();
  }

  /** Adds an admin without touching an existing row. Returns false when it already exists. */
  addAdmin(row: { identity: string; displayName: string | null; addedBy: string }): boolean {
    const result = this.db
      .insert(admin)
      .values({ identity: row.identity, displayName: row.displayName, notifyThread: null, addedBy: row.addedBy, addedAt: Date.now() })
      .onConflictDoNothing()
      .run();
    return result.changes > 0;
  }

  removeAdmin(identity: string): boolean {
    return this.db.delete(admin).where(eq(admin.identity, identity)).run().changes > 0;
  }

  // ---- bindings ----

  /** Whether bearbot-im may see the binding: its platform is registered (console only when enabled). */
  private visible(row: BindingRow): boolean {
    return this.options.platforms.has(row.platform);
  }

  binding(channelId: string): BindingRow | undefined {
    const row = this.db.select().from(binding).where(eq(binding.channelId, channelId)).get();
    return row && this.visible(row) ? row : undefined;
  }

  bindings(): BindingRow[] {
    return this.db
      .select()
      .from(binding)
      .orderBy(asc(binding.createdAt), asc(binding.channelId))
      .all()
      .filter((r) => this.visible(r));
  }

  wireBindings(): WireBinding[] {
    return this.bindings().map((r) => ({
      channel: r.channelId,
      name: r.channelName,
      platform: r.platform,
      trigger: r.trigger,
      prefix: r.prefix,
      replyMode: r.replyMode,
      interrupt: r.interrupt,
    }));
  }

  putBinding(row: {
    channelId: string;
    platform: string;
    channelName: string | null;
    trigger: Trigger;
    prefix: string | null;
    replyMode: ReplyMode;
    interrupt: Interrupt;
    createdBy: string;
  }): void {
    this.db
      .insert(binding)
      .values({ ...row, createdAt: Date.now() })
      .onConflictDoUpdate({
        target: binding.channelId,
        set: {
          channelName: row.channelName,
          trigger: row.trigger,
          prefix: row.prefix,
          replyMode: row.replyMode,
          interrupt: row.interrupt,
        },
      })
      .run();
  }

  deleteBinding(channelId: string): boolean {
    return this.db.delete(binding).where(eq(binding.channelId, channelId)).run().changes > 0;
  }

  /** DESIGN 6.2.4: the console channel is always bound while the console is enabled. */
  ensureConsoleBinding(): void {
    this.db
      .insert(binding)
      .values({
        channelId: CONSOLE_CHANNEL,
        platform: CONSOLE_PLATFORM,
        channelName: CONSOLE_CHANNEL_NAME,
        trigger: "all",
        prefix: null,
        replyMode: "final",
        interrupt: "queue",
        createdBy: CONSOLE_OPERATOR,
        createdAt: Date.now(),
      })
      .onConflictDoNothing()
      .run();
  }

  // ---- classifier ----

  classifierSetting(): ClassifierSettingRow | undefined {
    return this.db.select().from(classifierSetting).where(eq(classifierSetting.id, 1)).get();
  }

  classifier(): ClassifierInfo {
    const setting = this.classifierSetting();
    const rules = this.db
      .select()
      .from(classifierRule)
      .orderBy(asc(classifierRule.createdAt), asc(classifierRule.pattern))
      .all()
      .map((r) => ({ pattern: r.pattern, action: r.action }));
    return { enabled: setting ? setting.enabled : this.options.classifierEnabledDefault, rules };
  }

  setClassifierEnabled(enabled: boolean, by: string): void {
    this.db
      .insert(classifierSetting)
      .values({ id: 1, enabled, updatedBy: by, updatedAt: Date.now() })
      .onConflictDoUpdate({ target: classifierSetting.id, set: { enabled, updatedBy: by, updatedAt: Date.now() } })
      .run();
  }

  putRules(patterns: string[], action: "allow" | "deny", by: string): void {
    this.db.transaction((tx) => {
      for (const pattern of patterns) {
        tx.insert(classifierRule)
          .values({ pattern, action, createdBy: by, createdAt: Date.now() })
          .onConflictDoUpdate({ target: classifierRule.pattern, set: { action, createdBy: by, createdAt: Date.now() } })
          .run();
      }
    });
  }

  clearRules(patterns: string[]): { removed: string[]; missing: string[] } {
    return this.db.transaction((tx) => {
      const existing = new Set(
        tx
          .select({ pattern: classifierRule.pattern })
          .from(classifierRule)
          .where(inArray(classifierRule.pattern, patterns))
          .all()
          .map((r) => r.pattern),
      );
      if (existing.size > 0) {
        tx.delete(classifierRule).where(inArray(classifierRule.pattern, [...existing])).run();
      }
      return {
        removed: patterns.filter((p) => existing.has(p)),
        missing: patterns.filter((p) => !existing.has(p)),
      };
    });
  }

  // ---- maintenance notice ----

  /** Records that the channel got the maintenance notice; false when it already got it in this period. */
  takeMaintenanceNotice(channelId: string): boolean {
    return this.db.insert(maintenanceNotice).values({ channelId }).onConflictDoNothing().run().changes === 1;
  }

  /** The maintenance period ended: every channel gets the notice again in the next one. */
  clearMaintenanceNotices(): void {
    this.db.delete(maintenanceNotice).run();
  }

  // ---- bridge cursor ----

  ackedSeq(): number {
    return this.db.select().from(bridgeCursor).where(eq(bridgeCursor.id, 1)).get()?.ackedSeq ?? 0;
  }

  /**
   * bridge_cursor = max(current, seq); the completed seqs in `done` above it go to bridge_done, and bridge_done rows
   * the cursor has passed are deleted, in one transaction (DESIGN 6.8). Returns the new cursor.
   */
  ack(seq: number, done: readonly number[]): number {
    return this.db.transaction((tx) => {
      const next = Math.max(tx.select().from(bridgeCursor).where(eq(bridgeCursor.id, 1)).get()?.ackedSeq ?? 0, seq);
      tx.insert(bridgeCursor)
        .values({ id: 1, ackedSeq: next })
        .onConflictDoUpdate({ target: bridgeCursor.id, set: { ackedSeq: next } })
        .run();
      const above = done.filter((value) => value > next);
      if (above.length > 0) {
        tx.insert(bridgeDone).values(above.map((value) => ({ seq: value }))).onConflictDoNothing().run();
      }
      tx.delete(bridgeDone).where(lte(bridgeDone.seq, next)).run();
      return next;
    });
  }

  /** Seqs in bridge_done, ascending. */
  doneSeqs(): number[] {
    return this.db.select().from(bridgeDone).orderBy(asc(bridgeDone.seq)).all().map((row) => row.seq);
  }
}
