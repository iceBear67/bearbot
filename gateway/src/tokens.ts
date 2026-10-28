import { createHash, randomBytes } from "node:crypto";
import { base32nopad } from "@scure/base";
import { and, eq, gt, isNull } from "drizzle-orm";
import type { Db } from "./db/index.ts";
import { admin, adminToken } from "./db/schema.ts";

/** Text that looks like an admin token: one word starting with "bearbot-" (DESIGN 6.5 step 1). */
export const TOKEN_LIKE = /^bearbot-\S+$/i;

function hash(token: string): string {
  return createHash("sha256").update(token.toLowerCase()).digest("hex");
}

export type ClaimResult = "claimed" | "invalid";

/** One-time admin tokens (DESIGN 6.6). */
export class Tokens {
  constructor(private readonly db: Db) {}

  /** "bearbot-" + 26 base32 characters from 128 random bits; only the SHA-256 hash is stored. */
  create(ttlMs: number): { token: string; expiresAt: Date } {
    const token = `bearbot-${base32nopad.encode(randomBytes(16)).toLowerCase()}`;
    const now = Date.now();
    const expiresAt = now + ttlMs;
    this.db.insert(adminToken).values({ tokenHash: hash(token), createdAt: now, expiresAt }).run();
    return { token, expiresAt: new Date(expiresAt) };
  }

  /**
   * Consumes an unexpired, unused token and makes `identity` an admin whose notify_thread is `thread`.
   * Token use and admin insertion happen in one transaction.
   */
  claim(token: string, claimant: { identity: string; displayName: string | null; thread: string }): ClaimResult {
    const now = Date.now();
    return this.db.transaction((tx): ClaimResult => {
      const used = tx
        .update(adminToken)
        .set({ usedBy: claimant.identity, usedAt: now })
        .where(and(eq(adminToken.tokenHash, hash(token)), isNull(adminToken.usedBy), gt(adminToken.expiresAt, now)))
        .run();
      if (used.changes === 0) {
        return "invalid";
      }
      tx.insert(admin)
        .values({
          identity: claimant.identity,
          displayName: claimant.displayName,
          notifyThread: claimant.thread,
          addedBy: "token",
          addedAt: now,
        })
        .onConflictDoUpdate({
          target: admin.identity,
          set: { displayName: claimant.displayName, notifyThread: claimant.thread },
        })
        .run();
      return "claimed";
    });
  }
}
