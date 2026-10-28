import { pino, type Logger as PinoLogger } from "pino";
import type { Logger as ChatLogger } from "chat";

export type { PinoLogger };

/** JSON lines on stdout; bearbotd writes the gateway's stdout into logs/gateway.log. */
export function createLogger(level: string): PinoLogger {
  return pino({
    level,
    base: { component: "gateway" },
    serializers: { err: pino.stdSerializers.err, error: pino.stdSerializers.err },
  });
}

function fields(args: unknown[]): object {
  if (args.length === 0) {
    return {};
  }
  if (args.length === 1) {
    const [only] = args;
    if (only instanceof Error) {
      return { error: only };
    }
    if (typeof only === "object" && only !== null) {
      return only;
    }
  }
  return { args };
}

/** chat-sdk calls child("telegram") with a string and log methods as (message, ...args) (chat-sdk-core.md 2.3). */
export function toChatLogger(base: PinoLogger): ChatLogger {
  return {
    child: (prefix: string) => toChatLogger(base.child({ scope: prefix })),
    debug: (message: string, ...args: unknown[]) => base.debug(fields(args), message),
    info: (message: string, ...args: unknown[]) => base.info(fields(args), message),
    warn: (message: string, ...args: unknown[]) => base.warn(fields(args), message),
    error: (message: string, ...args: unknown[]) => base.error(fields(args), message),
  };
}

export type LogObserver = (level: "debug" | "info" | "warn" | "error", message: string, args: unknown[]) => void;

/** A chat logger that also reports every call to `observer` (used for connection status of the QQ adapter). */
export function observedLogger(inner: ChatLogger, observer: LogObserver): ChatLogger {
  return {
    child: (prefix: string) => observedLogger(inner.child(prefix), observer),
    debug: (message: string, ...args: unknown[]) => {
      inner.debug(message, ...args);
      observer("debug", message, args);
    },
    info: (message: string, ...args: unknown[]) => {
      inner.info(message, ...args);
      observer("info", message, args);
    },
    warn: (message: string, ...args: unknown[]) => {
      inner.warn(message, ...args);
      observer("warn", message, args);
    },
    error: (message: string, ...args: unknown[]) => {
      inner.error(message, ...args);
      observer("error", message, args);
    },
  };
}

export function errorText(e: unknown): string {
  if (e instanceof Error) {
    const code = (e as { code?: unknown }).code;
    return typeof code === "string" && code !== "" && !e.message.includes(code) ? `${code}: ${e.message}` : e.message;
  }
  return String(e);
}
