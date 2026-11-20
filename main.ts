// Launcher for the forge.
//
// The forge itself is a statically linked Go binary vendored in
// this repository (./forge). This script starts it as the forge's
// single process, forwards signals to it, and exits with its code,
// so the forge runs as one process with the same environment and
// arguments it was started with. No network, no package manager,
// and no other process is involved.
import { spawn } from "node:child_process";
import { accessSync, chmodSync, constants, copyFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, "forge");

// Resolve an executable path for the vendored binary.
//
// The binary may lose its executable bit when the repository is
// fetched from a host that does not preserve file modes (a file
// uploaded through the web UI lands as 0644), and the entry may run
// as a user who does not own the files, so chmod can fail too.
// Fall back to a copy in the temp directory, which the entry owns
// and can make executable.
function resolveBinary(): string {
  try {
    accessSync(binary, constants.X_OK);
    return binary;
  } catch {
    // Not executable as-is; try to fix the mode in place.
  }
  try {
    chmodSync(binary, 0o755);
    accessSync(binary, constants.X_OK);
    return binary;
  } catch {
    // The entry does not own the file (or the tree is read-only).
  }
  const dir = mkdtempSync(join(tmpdir(), "forge-"));
  const copy = join(dir, "forge");
  copyFileSync(binary, copy);
  chmodSync(copy, 0o755);
  accessSync(copy, constants.X_OK);
  return copy;
}

let executable: string;
try {
  executable = resolveBinary();
} catch {
  process.stderr.write(`forge: the vendored binary ${binary} is missing or not executable\n`);
  process.exit(1);
}

const child = spawn(executable, process.argv.slice(2), { stdio: "inherit" });

for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"] as const) {
  process.on(signal, () => child.kill(signal));
}

child.on("exit", (code, signal) => {
  if (executable !== binary) {
    try {
      rmSync(dirname(executable), { recursive: true, force: true });
    } catch {
      // Best effort cleanup only.
    }
  }
  if (signal !== null) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
});
