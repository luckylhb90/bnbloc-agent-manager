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
- **Install** (npm and Python agents): into its own folder, `<version>/` beside the
  others, checked (the command must report the version asked for) before anything
  points at it. The npm cache is its own; your global packages are untouched. Never
  elevated. A **Python** agent (`install.method: pip`, Hermes) is installed from
  **wheels only**: pip's own resolver names the package, its extras and every
  dependency as wheels with each one's sha256 (a dependency that has no wheel for
  the platform fails the install before anything is downloaded; nothing is ever
  built), the wheels are downloaded with `--require-hashes`, a virtual environment
  is made in the version's own folder and they are installed into it offline
  (`--no-index --find-links --require-hashes --no-deps`). No code of the package runs
  while it is installed, the same guarantee `npm --ignore-scripts` gives. pip runs
  without your own configuration or index (`PIP_*`, `PYTHONPATH` and a `pip.conf` are
  ignored); the interpreter is the newest of the catalog's range that is on your
  PATH (or in `uv`), and the page says which Python to install when there is none.
  The environment is built where it stays (a virtual environment writes its path into
  its commands), with an `.installing` marker that a finished install removes and a
  crash leaves for the next start to sweep. `requirements.lock` stays in the version
  folder: every wheel with its hash.
- **A room's conversation is never lost to an update.** A session keeps the
  program it was created with, and continues with it after an idle stop. An old
  version is therefore removed only when no live session needs it: the ACP core
  (0.2.12 or newer) publishes what its sessions run from, and a version it lists
  stays (the page says "kept for a room still using …"). With no word from the
  core (an older core, or none has run) nothing is removed. Remove is refused
  while a room still runs the agent. If a file is gone anyway, the core continues
  the conversation with the agent's current version and says so in its log.
- **Where to install: this computer or your sandbox, for every agent.** An agent
  that is not installed shows the same choice, *Where to install*, with both places
  on its row: *This computer* and *My sandbox*, each with what it means or, greyed
  out, why it cannot be chosen (never hidden). The choice is recorded and drives the
  store, the registration, the version looked for, update and remove; moving an agent
  is Remove, then Install in the other place. An organization can offer an agent only
  in the sandbox (`sandboxOnly`). Nothing of a sandbox install is installed or run on
  your computer: the files are unpacked (a platform package, `tarball`) or installed
  by this computer's npm for the guest's platform (`npm`: 10.5 or newer, `--os=linux
  --cpu=<guest> --libc=glibc`, **never a script**) into a folder of their own under
  the workspaces root, and the sandbox bridge gives each room's sandbox a read-only
  tree of hard links to that one copy (`bnbloc.sandbox.mounts/v1`, `perSpace`), so ten
  rooms cost one copy on disk. The ACP core launches it inside the sandbox
  (`"sandbox": "guest"`), never on your computer. It needs the desktop app with the
  sandbox on.

  A catalog entry's `sandbox` block says how: `method` (`tarball`, `npm` or `pip`),
  `package`, `bin`, `latest`, `python` and `dataEnv` (pip), `approxMB`, `note`, `register` (when the sandbox launch
  line differs from the host's), and `signIn`: `key` (it works with the member's own API key, which cannot be handed
  to the sandbox safely yet: it is still offered and runs with free or local models,
  and the choice says what is missing) or `login` (its only way in is an account login
  of its own, which the sandbox must never hold: not offered). `unavailable` is the whole block for an
  agent that cannot be offered, with the reason shown. OpenCode 1.x and OpenCode 2 are
  two products and two rows. Design:
  [`docs/prd/2026-10-26-sandbox-only-agents-design.md`](https://github.com/luckylhb90/bnbloc/blob/main/docs/prd/2026-10-26-sandbox-only-agents-design.md),
  [`docs/prd/2026-10-27-model-credentials.md`](https://github.com/luckylhb90/bnbloc/blob/main/docs/prd/2026-10-27-model-credentials.md)
  (how an API key will reach the sandbox without entering it).
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

`manager/catalog.json`: typed entries (an npm or Python package and its command, how a
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
