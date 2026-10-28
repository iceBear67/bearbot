import { type ChildProcessWithoutNullStreams, spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export type RpcRecord = Record<string, unknown> & { type: string };

export interface PiExit {
	code: number | null;
	signal: NodeJS.Signals | null;
}

function findRepoRoot(): string {
	let dir = dirname(fileURLToPath(import.meta.url));
	while (!existsSync(join(dir, "DESIGN.md"))) {
		const parent = dirname(dir);
		if (parent === dir) {
			throw new Error("DESIGN.md not found above the test directory");
		}
		dir = parent;
	}
	return dir;
}

export const repoRoot = findRepoRoot();
export const piBin = join(repoRoot, "node_modules", ".bin", "pi");

type Waiter = { predicate: (record: RpcRecord) => boolean; resolve: (record: RpcRecord) => void; reject: (error: Error) => void };

// A real pi process in RPC mode. stdout is split on LF only, as pi's own framing requires.
export class PiRpc {
	readonly records: RpcRecord[] = [];
	readonly exited: Promise<PiExit>;
	private readonly child: ChildProcessWithoutNullStreams;
	private stdoutBuffer = "";
	private stderrText = "";
	private exit: PiExit | undefined;
	private waiters: Waiter[] = [];

	constructor(args: string[], options: { cwd: string; env: Record<string, string> }) {
		this.child = spawn(piBin, ["--mode", "rpc", ...args], { cwd: options.cwd, env: options.env, stdio: ["pipe", "pipe", "pipe"] });
		this.child.stdout.setEncoding("utf8");
		this.child.stderr.setEncoding("utf8");
		this.child.stdout.on("data", (chunk: string) => this.onStdout(chunk));
		this.child.stderr.on("data", (chunk: string) => {
			this.stderrText += chunk;
		});
		this.exited = new Promise((resolve) => {
			this.child.on("exit", (code, signal) => {
				this.exit = { code, signal };
				for (const waiter of this.waiters) {
					waiter.reject(new Error(`pi exited (code ${code}, signal ${signal}) before the awaited record; stderr:\n${this.stderrText}`));
				}
				this.waiters = [];
				resolve(this.exit);
			});
		});
	}

	get stderr(): string {
		return this.stderrText;
	}

	send(command: Record<string, unknown>): void {
		this.child.stdin.write(`${JSON.stringify(command)}\n`);
	}

	waitFor(predicate: (record: RpcRecord) => boolean, timeoutMs: number): Promise<RpcRecord> {
		const found = this.records.find(predicate);
		if (found) {
			return Promise.resolve(found);
		}
		if (this.exit) {
			return Promise.reject(new Error(`pi already exited (code ${this.exit.code}); stderr:\n${this.stderrText}`));
		}
		return new Promise((resolve, reject) => {
			const timer = setTimeout(() => {
				this.waiters = this.waiters.filter((waiter) => waiter !== entry);
				reject(new Error(`no matching RPC record within ${timeoutMs} ms; stderr:\n${this.stderrText}`));
			}, timeoutMs);
			const entry: Waiter = {
				predicate,
				resolve: (record) => {
					clearTimeout(timer);
					resolve(record);
				},
				reject: (error) => {
					clearTimeout(timer);
					reject(error);
				},
			};
			this.waiters.push(entry);
		});
	}

	async request(command: Record<string, unknown> & { id: string }, timeoutMs: number): Promise<RpcRecord> {
		this.send(command);
		return this.waitFor((record) => record.type === "response" && record.id === command.id, timeoutMs);
	}

	async close(): Promise<PiExit> {
		this.child.stdin.end();
		return this.exited;
	}

	kill(): void {
		if (!this.exit) {
			this.child.kill("SIGKILL");
		}
	}

	private onStdout(chunk: string): void {
		this.stdoutBuffer += chunk;
		let newline = this.stdoutBuffer.indexOf("\n");
		while (newline !== -1) {
			const line = this.stdoutBuffer.slice(0, newline);
			this.stdoutBuffer = this.stdoutBuffer.slice(newline + 1);
			const record = JSON.parse(line) as RpcRecord;
			this.records.push(record);
			const matched = this.waiters.filter((waiter) => waiter.predicate(record));
			this.waiters = this.waiters.filter((waiter) => !matched.includes(waiter));
			for (const waiter of matched) {
				waiter.resolve(record);
			}
			newline = this.stdoutBuffer.indexOf("\n");
		}
	}
}

export function commandNames(response: RpcRecord): string[] {
	const data = response.data as { commands: { name: string }[] };
	return data.commands.map((command) => command.name);
}
