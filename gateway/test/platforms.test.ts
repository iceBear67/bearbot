// Platform adapters inside the built gateway whose first connection to the Telegram Bot API or QQ open platform fails.
import { afterEach, beforeAll, describe, expect, test } from "vitest";
import {
  GatewayProcess,
  connectBridge,
  connectDaemon,
  makeEnv,
  removeEnv,
  requireBuild,
  sleep,
  type Env,
  type RpcPeer,
} from "./helpers.ts";

beforeAll(() => requireBuild());

const cleanups: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  while (cleanups.length > 0) await cleanups.pop()!();
});

async function start(env: Env): Promise<GatewayProcess> {
  const gw = new GatewayProcess(env);
  cleanups.push(() => gw.kill());
  await gw.ready();
  return gw;
}

async function waitPlatform(ctl: RpcPeer, name: string, accept: (p: any) => boolean, timeoutMs: number): Promise<any> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const { platforms } = await ctl.request("platforms.status", {});
    const p = platforms.find((x: any) => x.name === name);
    if (p && accept(p)) return p;
    if (Date.now() > deadline) throw new Error(`platform ${name} status: ${JSON.stringify(p)}`);
    await sleep(250);
  }
}

describe("a failed first connection does not stop the gateway (contracts 9.4)", () => {
  test("telegram with an invalid token: recorded as error and retried, console keeps working", async () => {
    const env = makeEnv({ platforms: { telegram: { botToken: "123456:invalid-token-for-bearbot-tests" } } });
    cleanups.push(() => removeEnv(env));
    const gw = await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const tg = await waitPlatform(ctl, "telegram", (p) => p.state === "error", 30_000);
    expect(tg.lastError).toEqual(expect.any(String));
    expect(tg.lastErrorAt).toEqual(expect.any(String));
    await gw.waitLog(/"platform":"telegram".*"msg":"platform connection failed"/);

    const { peer, hello } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    expect(Object.keys(hello.self).sort()).toEqual(["console", "telegram"]);
    gw.type("console still works");
    await peer.waitMessage((m) => m.text === "console still works");

    // A second attempt happens after the 1 s backoff.
    await gw.waitLog(/"retryInMs":2000/, 20_000);
    gw.child.kill("SIGTERM");
    await gw.exited;
    expect(gw.exitCode).toBe(0);
  });

  test("qq with made-up credentials: recorded as error and retried", async () => {
    const env = makeEnv({ platforms: { qq: { appId: "102000000", clientSecret: "not-a-real-secret", sandbox: true, markdown: true } } });
    cleanups.push(() => removeEnv(env));
    const gw = await start(env);
    const ctl = await connectDaemon(env);
    cleanups.push(() => void ctl.close());
    const qq = await waitPlatform(ctl, "qq", (p) => p.state === "error", 30_000);
    expect(qq.lastError).toEqual(expect.any(String));
    const { peer, hello } = await connectBridge(env);
    cleanups.push(() => void peer.close());
    expect(hello.self.qq).toBe("bearbot");
    gw.type("ok");
    await peer.waitMessage((m) => m.text === "ok");
  });
});
