// Launcher for the forge.
//
// The forge itself is a statically linked Go binary vendored in
// this repository (./forge). This script starts it as the forge's
// single process, forwards signals to it, and exits with its code,
// so the forge runs as one process with the same environment and
// arguments it was started with. No network, no package manager,
// and no other process is involved.
import { spawn } from "node:child_process";
import { accessSync, constants } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, "forge");

try {
  accessSync(binary, constants.X_OK);
} catch {
  process.stderr.write(`forge: the vendored binary ${binary} is missing or not executable\n`);
  process.exit(1);
}

const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });

for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"] as const) {
  process.on(signal, () => child.kill(signal));
}

child.on("exit", (code, signal) => {
  if (signal !== null) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
});
