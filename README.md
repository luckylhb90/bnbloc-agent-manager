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
- **A room's conversation is never lost to an update.** A session keeps the
  program it was created with, and continues with it after an idle stop. An old
  version is therefore removed only when no live session needs it: the ACP core
  (0.2.12 or newer) publishes what its sessions run from, and a version it lists
  stays (the page says "kept for a room still using …"). With no word from the
  core (an older core, or none has run) nothing is removed. Remove is refused
  while a room still runs the agent. If a file is gone anyway, the core continues
  the conversation with the agent's current version and says so in its log.
- **Where to install: this computer or your sandbox.** An agent whose catalog entry
  has a `sandbox` block offers both on its row, and the choice is made when you
  install it: *Install on this computer* or *Install in my sandbox* (with its size
  and what differs). Nothing of a sandbox install is installed or run on your
  computer: the manager downloads the package's tarball, checks it against the
  registry's sha512, unpacks it (no npm, none of the package's scripts) into a
  folder of its own under the workspaces root, and tells the sandbox bridge
  (`bnbloc.sandbox.mounts/v1`, `perSpace`): each room's sandbox gets a tree of hard
  links to that one copy, read-only, so ten rooms cost one copy on disk (225 MB
  for OpenCode 2). The ACP core launches it inside the sandbox (`"sandbox":
  "guest"`) and never on your computer. The choice is recorded; moving an agent is
  Remove, then Install in the other place. An update in the sandbox is a click (a
  conversation continues with the new version), never automatic; Remove is refused
  while a room may be running it. An organization can offer an agent only in the
  sandbox (`sandboxOnly`). It needs the desktop app with the sandbox on. The
  entry names the package for the sandbox's image (`sandbox.package`, `{cpu}` is
  arm64 or x64; the image is Debian, so glibc builds) and, when the newest
  generation ships under another name, `sandbox.latest`. Only self-contained
  packages work (a program with its files); an agent that needs `npm install` of
  its dependencies is not offered the sandbox yet. Design:
  [`docs/prd/2026-10-26-sandbox-only-agents-design.md`](https://github.com/luckylhb90/bnbloc/blob/main/docs/prd/2026-10-26-sandbox-only-agents-design.md).
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

Two settings add agents without a new build (Components → Settings). Each agent
is a JSON object, on one line or written over several (objects are cut out by
their braces, so a pasted, pretty-printed object works):

- **My own agents** (`myAgents`, a member's, kept on their computer): a command
  name or an absolute path, version arguments and how to read the version, an
  install line for an agent that is not on the computer (`installArgs`, run with
  `sh` unless `installCommand` names another command) and an update command (the
  page shows the exact command before the click), and an ACP launch line to give
  it to rooms. A line with the id of a
  catalog agent and only `command` and `versionArgs` points that agent at another
  command or path (an agent installed where no PATH reaches).
- **More agents to recognise** (`orgAgents`, an organization's, for every member):
  recognition only, a bare command name and one version flag (`--version`, `-v`,
  `-V`, `version`), and words for the member. No install or update command, no ACP
  launch line, and no change to a catalog entry: a setting must not make members'
  computers run what an admin typed. An organization that needs entries that run
  commands makes its own build of the manager (`Catalog.With`, package `app`) and
  signs it as its own package.

An agent looks like this (all fields but `id` and `command` are optional):

```json
{"id":"corp","name":"Corp CLI","command":"corp","versionArgs":["--version"],
 "versionParse":"v(\\d+\\.\\d+\\.\\d+)","updateArgs":["update"],"acpArgs":["acp"],
 "installHint":"Install it from the portal.","signIn":"Run `corp login`."}
```

`updateCommand` (a bare command name, such as `brew`) runs `updateArgs` instead of
the agent itself. An install line is typed as the member would type it, for
example `"installArgs":["-c","curl -fsSL https://example.com/install.sh | bash"]`;
what it installs must then be on the PATH the app sees, or the page says it is not. An object that does not fit, and text outside any object, is named on the page and
left out.

You do not have to write one by hand: the settings form's **Fill from a note**
turns a pasted install note (a README section, an email) into the object, and
you check it before saving. It reads the settings' help text, which therefore
carries a working example (a test holds each example to the rules).

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
