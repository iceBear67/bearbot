import { chmodSync, chownSync, lstatSync, unlinkSync } from "node:fs";
import type { Server } from "node:net";

/** Removes a socket left by a previous gateway process; any other file at the path is an error. */
function removeStaleSocket(path: string): void {
  let st;
  try {
    st = lstatSync(path);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ENOENT") {
      return;
    }
    throw e;
  }
  if (!st.isSocket()) {
    throw new Error(`${path} exists and is not a socket`);
  }
  unlinkSync(path);
}

/**
 * Listens on a unix socket with the given mode (contracts 3). The umask is narrowed while the socket
 * file is created, so it never exists with wider permissions.
 */
export async function listenUnix(server: Server, path: string, mode: number, gid?: number): Promise<void> {
  removeStaleSocket(path);
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    // The socket file is bound synchronously inside listen(); restore the umask right after it.
    const previous = process.umask(0o777 & ~mode);
    try {
      server.listen(path, () => {
        server.off("error", reject);
        resolve();
      });
    } finally {
      process.umask(previous);
    }
  });
  chmodSync(path, mode);
  if (gid !== undefined) {
    chownSync(path, process.getuid!(), gid);
  }
}

export function closeServer(server: Server): Promise<void> {
  return new Promise((resolve, reject) => {
    if (!server.listening) {
      resolve();
      return;
    }
    server.close((e) => (e ? reject(e) : resolve()));
  });
}
