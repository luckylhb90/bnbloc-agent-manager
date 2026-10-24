# bnbloc-agent-manager

The **agent manager** for [bnbloc](https://github.com/luckylhb90/bnbloc): see
the coding agents on your computer, install the ones its catalog can, keep them
current, and give them to your rooms. It is a signed desktop **plugin
component** with its own page; nothing is installed or changed until you click,
unless you turned automatic updates on. Design record and review:
[`docs/prd/2026-10-24-agent-manager.md`](https://github.com/luckylhb90/bnbloc/blob/main/docs/prd/2026-10-24-agent-manager.md).

```
the page (a bundle, Components → Agent manager → Open)
   │ host messages only (no network)
   ▼
desktop relay ──loopback + per-launch bearer──▶ bnbloc-agent-manager
   catalog (signed, in the package) → detect · check · install · update · roll back · remove
   store:  <data dir>/agents/<id>/<version>/      state.json
   │ writes, atomically
   ▼
<acp home>/agents.d/agent-manager.json  ──▶  the ACP core (bnbloc-acp) reloads it
```

## What it does

- **Lists the catalog** with what is installed (by the manager, or found on your
  computer and how), the newest version, and what you can do.
- **Install** (npm agents): into its own folder, `<version>/` beside the others,
  checked (the command must report the version asked for) before anything points
  at it. The npm cache is its own; your global packages are untouched. Never
  elevated.
- **Update** installs the new version beside the old, checks it, then switches
  the registration. **Roll back** switches it back (the previous version is
  kept). A running agent keeps running from its old files; nothing is signalled.
- **An install you made yourself** is updated only by the method the catalog
  names for how it was found (the agent's own updater, `npm install -g`), on your
  click, after showing the exact command. It is never removed.
- **Use in rooms** gives an agent to the ACP core (a file in its `agents.d`
  directory). Your own `agents.json` always wins on a name.
- **An agent and its ACP adapter are two rows.** The agent is the command you
  installed (Claude Code's `claude`, `codex`): found, versioned and updated by
  its own method. Rooms run another program, the agent's ACP adapter, which is its
  own row under it (installed and registered by the manager); an agent that
  speaks ACP itself (Gemini CLI, OpenCode, Hermes) has no adapter row.
- Hermes and pi are in the first batch; Hermes installs with its own installer
  (the page shows how) and is managed after that.

## Policy (component settings)

Organization: `allowed` (agent ids offered), `pinned` (`id@version`),
`holdDays` (a version younger than this is not offered). Member: `autoUpdate`.
An organization cannot add a catalog entry through a setting: the catalog is
signed with the package, because it decides what runs on members' computers.

## Command line

```sh
bnbloc-agent-manager list
bnbloc-agent-manager install|update|rollback|remove|check|register|unregister <id>
bnbloc-agent-manager serve --page      # the API, and the page at /  (development)
```

`--data-dir`, `--acp-home` and `--settings` default to the user config folder,
the ACP core's home and none.

## Configuring more agents

Two settings add agents without a new build (Components → Settings), one agent
per line as JSON:

- **My own agents** (`myAgents`, a member's, kept on their computer): a command
  name or an absolute path, version arguments and how to read the version, an
  update command and its arguments (the page shows the exact command before the
  click), and an ACP launch line to give it to rooms. A line with the id of a
  catalog agent and only `command` and `versionArgs` points that agent at another
  command or path (an agent installed where no PATH reaches).
- **More agents to recognise** (`orgAgents`, an organization's, for every member):
  recognition only, a bare command name and one version flag (`--version`, `-v`,
  `-V`, `version`), and words for the member. No update command, no ACP launch
  line, and no change to a catalog entry: a setting must not make members'
  computers run what an admin typed. An organization that needs entries that run
  commands makes its own build of the manager (`Catalog.With`, package `app`) and
  signs it as its own package.

A line that does not fit is named on the page and left out.

## Catalog

`manager/catalog.json`: typed entries (an npm package and its command, how a
version is read, how an existing install is recognised, an agent's own updater,
the ACP launch line, the sign-in hint). There is no shell method: every method
is code that verifies what it did. An entry's facts are probed against the real
agent before it ships.

## Build and publish

```sh
go test -race ./...
go build -ldflags "-X main.version=0.1.0" -o dist/bnbloc-agent-manager-darwin-arm64 .   # GOOS/GOARCH per platform
bnbloc components publish-package --package package.json --binary darwin-arm64=dist/... --rollout optin
```

The component installs software, so it ships `optin`.
