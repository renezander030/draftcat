# Contributing to draftcat

PRs welcome — bug fixes and new built-in actions. Keep the zero-dependency,
one-binary spirit.

## Dev setup

Requires Go 1.25.

```bash
make check   # fmt-check + lint + fast tests + build
make test    # full suite
```

## Give your agent the map (optional)

draftcat's promise is that the LLM can't fire actions — every outbound step passes
`validateOutput` and `SendForApproval`, gated by the `knownActionNames` allowlist,
all running through `runPipeline`. Before you change that spine, see what depends on
it. [`pi-codegraph`](https://github.com/renezander030/pi-codegraph) hands your coding
agent a call-graph of the codebase so it stops re-reading the whole repo each session:

```bash
pi-codegraph trust --repo . --label draftcat
pi-codegraph index --repo .
pi-codegraph arch -H                          # routes, packages, the busiest functions
pi-codegraph trace validateOutput --inbound   # every path that hits the output-governance gate
pi-codegraph impact                           # before touching the pipeline/governance core, what breaks
```

Optional and external — nothing in the project depends on it.
