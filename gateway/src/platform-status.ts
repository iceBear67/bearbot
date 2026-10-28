import { errorText } from "./log.ts";
import type { PlatformState, PlatformStatus } from "./types.ts";

/** Connection status of one platform for `platforms.status` and `/bb status`. */
export class StatusRecorder {
  private state: PlatformState;
  private since: Date | null;
  private lastDisconnectAt: Date | null = null;
  private lastReconnectAt: Date | null = null;
  private lastError: string | null = null;
  private lastErrorAt: Date | null = null;
  private everConnected = false;

  constructor(
    readonly name: string,
    initial: PlatformState,
  ) {
    this.state = initial;
    this.since = new Date();
  }

  get current(): PlatformState {
    return this.state;
  }

  set(state: PlatformState): void {
    if (state !== this.state) {
      this.state = state;
      this.since = new Date();
    }
  }

  connecting(): void {
    if (this.state !== "connected") {
      this.set("connecting");
    }
  }

  connected(): void {
    if (this.state === "connected") {
      return;
    }
    if (this.everConnected) {
      this.lastReconnectAt = new Date();
    }
    this.everConnected = true;
    this.set("connected");
  }

  /** The connection was lost or could not be made; the adapter retries by itself. */
  disconnected(error?: unknown): void {
    if (this.state === "connected") {
      this.lastDisconnectAt = new Date();
    }
    if (error !== undefined) {
      this.recordError(error);
    }
    this.set("disconnected");
  }

  /** A failure that needs attention (authentication, terminal close codes, failed first connection). */
  failed(error: unknown): void {
    if (this.state === "connected") {
      this.lastDisconnectAt = new Date();
    }
    this.recordError(error);
    this.set("error");
  }

  recordError(error: unknown): void {
    this.lastError = typeof error === "string" ? error : errorText(error);
    this.lastErrorAt = new Date();
  }

  snapshot(): PlatformStatus {
    const iso = (d: Date | null) => (d ? d.toISOString() : null);
    return {
      name: this.name,
      state: this.state,
      since: iso(this.since),
      lastDisconnectAt: iso(this.lastDisconnectAt),
      lastReconnectAt: iso(this.lastReconnectAt),
      lastError: this.lastError,
      lastErrorAt: iso(this.lastErrorAt),
    };
  }
}
