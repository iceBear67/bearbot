import { readFileSync } from "node:fs";
import { parseArgs } from "node:util";

/** gateway.json written by bearbotd (contracts section 3). Durations are integer milliseconds. */
export type GatewayConfig = {
  logLevel: "debug" | "info" | "warn" | "error";
  userName: string;
  gatewaySocket: string;
  ctlSocket: string;
  socketGroupGid: number;
  database: string;
  consoleFilesDir: string;
  workspaceDir: string;
  tokenTtlMs: number;
  replayWindowMs: number;
  messageRetentionMs: number;
  attachmentTimeoutMs: number;
  imageMaxBytes: number;
  maintenanceNotice: string;
  classifierEnabledDefault: boolean;
  http: { listen: string };
  platforms: {
    telegram?: { botToken: string };
    qq?: { appId: string; clientSecret: string; sandbox: boolean };
  };
  console?: { inputFd: number; outputFd: number };
};

const LOG_LEVELS = ["debug", "info", "warn", "error"] as const;

class ConfigError extends Error {
  constructor(file: string, message: string) {
    super(`${file}: ${message}`);
    this.name = "ConfigError";
  }
}

type Obj = Record<string, unknown>;

function isObj(v: unknown): v is Obj {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

class Reader {
  constructor(private readonly file: string) {}

  obj(parent: Obj, key: string, path: string): Obj {
    const v = parent[key];
    if (!isObj(v)) {
      throw new ConfigError(this.file, `${path}${key} must be an object`);
    }
    return v;
  }

  str(parent: Obj, key: string, path: string, allowEmpty = false): string {
    const v = parent[key];
    if (typeof v !== "string" || (!allowEmpty && v === "")) {
      throw new ConfigError(this.file, `${path}${key} must be a ${allowEmpty ? "" : "non-empty "}string`);
    }
    return v;
  }

  int(parent: Obj, key: string, path: string, min: number): number {
    const v = parent[key];
    if (typeof v !== "number" || !Number.isInteger(v) || v < min) {
      throw new ConfigError(this.file, `${path}${key} must be an integer >= ${min}`);
    }
    return v;
  }

  bool(parent: Obj, key: string, path: string): boolean {
    const v = parent[key];
    if (typeof v !== "boolean") {
      throw new ConfigError(this.file, `${path}${key} must be a boolean`);
    }
    return v;
  }
}

export function parseConfig(file: string, text: string): GatewayConfig {
  const raw: unknown = JSON.parse(text);
  if (!isObj(raw)) {
    throw new ConfigError(file, "top level must be an object");
  }
  const r = new Reader(file);
  const logLevel = r.str(raw, "logLevel", "");
  if (!(LOG_LEVELS as readonly string[]).includes(logLevel)) {
    throw new ConfigError(file, `logLevel must be one of ${LOG_LEVELS.join(", ")}`);
  }
  const http = r.obj(raw, "http", "");
  const platformsRaw = r.obj(raw, "platforms", "");
  const platforms: GatewayConfig["platforms"] = {};
  for (const name of Object.keys(platformsRaw)) {
    if (name !== "telegram" && name !== "qq") {
      throw new ConfigError(file, `platforms.${name} is not a supported platform`);
    }
  }
  if (platformsRaw.telegram !== undefined) {
    const tg = r.obj(platformsRaw, "telegram", "platforms.");
    platforms.telegram = { botToken: r.str(tg, "botToken", "platforms.telegram.") };
  }
  if (platformsRaw.qq !== undefined) {
    const qq = r.obj(platformsRaw, "qq", "platforms.");
    platforms.qq = {
      appId: r.str(qq, "appId", "platforms.qq."),
      clientSecret: r.str(qq, "clientSecret", "platforms.qq."),
      sandbox: r.bool(qq, "sandbox", "platforms.qq."),
    };
  }
  let consoleCfg: GatewayConfig["console"];
  if (raw.console !== undefined) {
    const c = r.obj(raw, "console", "");
    consoleCfg = { inputFd: r.int(c, "inputFd", "console.", 0), outputFd: r.int(c, "outputFd", "console.", 0) };
  }
  return {
    logLevel: logLevel as GatewayConfig["logLevel"],
    userName: r.str(raw, "userName", ""),
    gatewaySocket: r.str(raw, "gatewaySocket", ""),
    ctlSocket: r.str(raw, "ctlSocket", ""),
    socketGroupGid: r.int(raw, "socketGroupGid", "", 0),
    database: r.str(raw, "database", ""),
    consoleFilesDir: r.str(raw, "consoleFilesDir", ""),
    workspaceDir: r.str(raw, "workspaceDir", ""),
    tokenTtlMs: r.int(raw, "tokenTtlMs", "", 1),
    replayWindowMs: r.int(raw, "replayWindowMs", "", 0),
    messageRetentionMs: r.int(raw, "messageRetentionMs", "", 1),
    attachmentTimeoutMs: r.int(raw, "attachmentTimeoutMs", "", 1),
    imageMaxBytes: r.int(raw, "imageMaxBytes", "", 0),
    maintenanceNotice: r.str(raw, "maintenanceNotice", ""),
    classifierEnabledDefault: r.bool(raw, "classifierEnabledDefault", ""),
    http: { listen: r.str(http, "listen", "http.", true) },
    platforms,
    console: consoleCfg,
  };
}

/** Reads `--config <path>` from the command line and loads the file. */
export function loadConfigFromArgs(argv: string[]): GatewayConfig {
  const { values } = parseArgs({ args: argv, options: { config: { type: "string" } }, strict: true });
  if (!values.config) {
    throw new Error("usage: main.js --config <gateway.json>");
  }
  return parseConfig(values.config, readFileSync(values.config, "utf8"));
}
