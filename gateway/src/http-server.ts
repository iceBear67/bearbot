import type { AddressInfo } from "node:net";
import { serve, type ServerType } from "@hono/node-server";
import type { Chat } from "chat";
import { Hono } from "hono";
import type { PinoLogger } from "./log.ts";

/** "host:port", ":port" or "port". */
function parseListen(listen: string): { hostname: string; port: number } {
  const m = /^(?:(.*):)?(\d+)$/.exec(listen);
  if (!m) {
    throw new Error(`http.listen must be "host:port", ":port" or "port", got ${JSON.stringify(listen)}`);
  }
  const host = m[1] ?? "";
  return { hostname: host === "" ? "0.0.0.0" : host.replace(/^\[(.*)\]$/, "$1"), port: Number(m[2]) };
}

/** Webhook endpoint `POST /webhooks/<platform>`, started only when `[gateway.http] listen` is set. */
export class HttpServer {
  private server: ServerType | null = null;

  constructor(
    private readonly listen: string,
    private readonly bot: Chat,
    private readonly platforms: ReadonlySet<string>,
    private readonly logger: PinoLogger,
  ) {}

  async start(): Promise<void> {
    const app = new Hono();
    app.post("/webhooks/:platform", async (c) => {
      const platform = c.req.param("platform");
      const handler = this.bot.webhooks[platform];
      if (!this.platforms.has(platform) || !handler) {
        return c.json({ error: `platform ${platform} is not enabled` }, 404);
      }
      return await handler(c.req.raw);
    });
    const { hostname, port } = parseListen(this.listen);
    const server = serve({ fetch: app.fetch, hostname, port });
    this.server = server;
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.once("listening", () => {
        server.off("error", reject);
        resolve();
      });
    });
    this.logger.info({ address: server.address() as AddressInfo }, "webhook HTTP server listening");
  }

  async close(): Promise<void> {
    const server = this.server;
    if (!server) {
      return;
    }
    await new Promise<void>((resolve, reject) => server.close((e) => (e ? reject(e) : resolve())));
  }
}
