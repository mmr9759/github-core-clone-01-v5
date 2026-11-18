// Forge launcher (Node entry point).
//
// The real server is the vendored static binary next to this file. This
// launcher only spawns it with the same environment and stdio, forwards
// termination signals, and exits with the child's code — so the trial's
// register/done lifecycle is owned entirely by the binary.
//
// Layout of this repository:
//   forge        the static server binary (built from the Go sources)
//   *.go         the server: raw TCP HTTP/1.1, in-memory store, git
//                smart-HTTP, REST v3, GraphQL v4, MCP
//   main.ts      this launcher (the trial runs `node main.ts`)
//   README.md    design notes

import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";

const dir = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(dir, "forge");

const child = spawn(binary, [], { stdio: "inherit", env: process.env });

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => child.kill(sig));
}

child.on("error", (err) => {
  console.error("forge launcher: failed to start binary:", err.message);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  process.exit(signal ? 1 : code ?? 1);
});
