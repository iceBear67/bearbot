import { and, asc, desc, eq, gt, inArray, lt, notInArray, sql } from "drizzle-orm";
import type { Db } from "./db/index.ts";
import { bridgeDone, messageLog } from "./db/schema.ts";
import type { InboundPayload, MessageKind, OutboundPayload } from "./types.ts";

export type MessageRow = typeof messageLog.$inferSelect;

export type InboundRow = Omit<MessageRow, "payload" | "direction"> & { direction: "in"; payload: InboundPayload };

/** message_log (DESIGN 6.4): inbound events of bound channels and every outbound message. */
export class MessageLog {
  constructor(private readonly db: Db) {}

  /** Allocates the next seq and stores the row in one transaction (DESIGN 6.5 step 6). */
  insertInbound(row: {
    kind: MessageKind;
    threadId: string;
    channelId: string;
    messageId: string;
    payload: InboundPayload;
    createdAt: number;
    quoteId: string | null;
  }): number {
    return this.db.transaction((tx) => {
      const inserted = tx
        .insert(messageLog)
        .values({ direction: "in", ...row })
        .returning({ seq: messageLog.seq })
        .get();
      return inserted.seq;
    });
  }

  insertOutbound(row: { threadId: string; channelId: string; messageId: string; payload: OutboundPayload; quoteId: string | null }): number {
    return this.db.transaction((tx) => {
      const inserted = tx
        .insert(messageLog)
        .values({ direction: "out", kind: "message", createdAt: Date.now(), ...row })
        .returning({ seq: messageLog.seq })
        .get();
      return inserted.seq;
    });
  }

  updatePayload(seq: number, payload: InboundPayload): void {
    this.db.update(messageLog).set({ payload }).where(eq(messageLog.seq, seq)).run();
  }

  /** Inbound rows after `seq`, oldest first. */
  inboundAfter(seq: number, limit: number): InboundRow[] {
    return this.db
      .select()
      .from(messageLog)
      .where(and(eq(messageLog.direction, "in"), gt(messageLog.seq, seq)))
      .orderBy(asc(messageLog.seq))
      .limit(limit)
      .all() as InboundRow[];
  }

  /** Inbound rows after `seq` that bearbot-im has not completed (not in bridge_done), in seq order (DESIGN 6.8). */
  inboundToSend(seq: number, limit: number): InboundRow[] {
    return this.db
      .select()
      .from(messageLog)
      .where(and(eq(messageLog.direction, "in"), gt(messageLog.seq, seq), notInArray(messageLog.seq, this.db.select({ seq: bridgeDone.seq }).from(bridgeDone))))
      .orderBy(asc(messageLog.seq))
      .limit(limit)
      .all() as InboundRow[];
  }

  /** Last `limit` rows of a thread or a channel before seq `before`, oldest first. */
  history(target: { threadId?: string; channelId?: string }, limit: number, before?: number): MessageRow[] {
    const where = and(
      target.threadId !== undefined ? eq(messageLog.threadId, target.threadId) : eq(messageLog.channelId, target.channelId!),
      before !== undefined ? lt(messageLog.seq, before) : undefined,
    );
    return this.db.select().from(messageLog).where(where).orderBy(desc(messageLog.seq)).limit(limit).all().reverse();
  }

  /** The newest message or command row of a thread with this platform message id (action and reaction rows carry the id of the message they belong to). */
  findMessage(threadId: string, messageId: string): MessageRow | undefined {
    return this.db
      .select()
      .from(messageLog)
      .where(
        and(
          eq(messageLog.threadId, threadId),
          eq(messageLog.messageId, messageId),
          inArray(messageLog.kind, ["message", "command"]),
        ),
      )
      .orderBy(desc(messageLog.seq))
      .limit(1)
      .get();
  }

  /** The newest row of a thread with this QQ quote index. */
  findByQuoteId(threadId: string, quoteId: string): MessageRow | undefined {
    return this.db
      .select()
      .from(messageLog)
      .where(and(eq(messageLog.threadId, threadId), eq(messageLog.quoteId, quoteId)))
      .orderBy(desc(messageLog.seq))
      .limit(1)
      .get();
  }

  /** Threads of a channel by last activity, newest first. */
  threadsOf(channelId: string, limit: number): { thread: string; lastActiveAt: number }[] {
    return this.db
      .select({ thread: messageLog.threadId, lastActiveAt: sql<number>`max(${messageLog.createdAt})` })
      .from(messageLog)
      .where(eq(messageLog.channelId, channelId))
      .groupBy(messageLog.threadId)
      .orderBy(desc(sql`max(${messageLog.createdAt})`))
      .limit(limit)
      .all();
  }

  /** Rows whose attachment downloads were cut off by a gateway restart. */
  pendingInbound(): InboundRow[] {
    return this.db
      .select()
      .from(messageLog)
      .where(and(eq(messageLog.direction, "in"), sql`json_extract(${messageLog.payload}, '$.pending') = 1`))
      .orderBy(asc(messageLog.seq))
      .all() as InboundRow[];
  }

  /** Retention purge (DESIGN 6.4). */
  deleteOlderThan(cutoff: number): number {
    return this.db.delete(messageLog).where(lt(messageLog.createdAt, cutoff)).run().changes;
  }
}
