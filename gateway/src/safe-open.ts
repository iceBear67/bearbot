import { closeSync, constants, fstatSync, openSync, readlinkSync, type Stats } from "node:fs";

/** An open descriptor, the path the kernel opened (without symlinks) and its fstat. */
export type OpenedFile = { fd: number; path: string; stats: Stats };

/**
 * Opens a file in a directory the agent can write (the workspace, DESIGN 15). The gateway runs as bearbot-gw,
 * so a symlink or a renamed directory placed by the agent must not lead it to its own files: a symlink as the
 * last path component fails with ELOOP, a FIFO does not block, and the result carries the path of the opened
 * file from /proc/self/fd. The caller compares `path` with the location it expects and checks
 * `stats.isFile()` before it reads or writes `fd`, and closes `fd`.
 */
export function openNoFollow(path: string, flags: number, mode?: number): OpenedFile {
  const fd = openSync(path, flags | constants.O_NOFOLLOW | constants.O_NONBLOCK, mode);
  try {
    return { fd, path: readlinkSync(`/proc/self/fd/${fd}`), stats: fstatSync(fd) };
  } catch (e) {
    closeSync(fd);
    throw e;
  }
}
