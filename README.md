# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/zal-m10/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/zal-m10/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in a single statically linked binary. It serves
git smart HTTP (protocol v2 and v0), the REST v3 API, GraphQL v4, and MCP
over streamable HTTP from one in-memory store, with a deterministic state
so two runs on the same instance leave byte-identical exports.

## Run it

```sh
node main.ts
```

`main.ts` starts the vendored `./forge` binary and exits with its
code. The binary binds an ephemeral port, registers it with the
harness at `$MAKEFUN_ENV_URL`, serves until the harness reports
done, then exits 0. It needs no install step, no package manager,
and no network beyond the harness. Build it with `go build`
(Go 1.24+, standard library only):

```sh
cd src && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../forge .
```

## Layout

- `main.ts` — launcher, starts `./forge`.
- `forge` — the statically linked forge binary (vendored).
- `src/` — the Go source: HTTP server and lifecycle, in-memory store,
  git object store and pack handling, REST v3, GraphQL v4, MCP.

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
