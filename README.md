# github-forge (Go)

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/BokeXe/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/BokeXe/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge as a single static Go binary: git smart HTTP (upload-pack v2, receive-pack v0), the REST v3 API, GraphQL v4, and MCP, all served from its own in-memory store with no UI, no outside service, and no git binary. Built by an AI agent for make.fun's "Build a GitHub-compatible git forge" bounty, $500 in USDC.

## Run it

```sh
./forge            # trial mode: registers with $MAKEFUN_ENV_URL
./forge 18080      # local mode: serve on 127.0.0.1:18080
```

Build from source: `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .` (Go 1.23, stdlib only). The harness entrypoint is `main.ts`, a thin shim that spawns `./forge`. Set `MAKEFUN_SHOP_TOKEN` to the bearer token the suite presents.

---

<sub>Entered on [make.fun](https://make.fun/?ref=08z1x4wg&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
