# claude-pack

Bundle a project directory **together with its Claude Code sessions and memories** into a single archive — and restore all of it on the same or a different machine, even when the project lands at a different path.

Claude Code keys its sessions and memories to the *absolute path* of your project (`~/.claude/projects/<encoded-path>/`). Copy a project to another machine and your conversation history and memories don't follow. `claude-pack` fixes that: it bundles everything, and on import it re-encodes the storage directory for the new location and rewrites every path reference inside the session and memory files, so `claude --resume` just works.

## Install

Download a single static binary for your platform from the [releases page](../../releases), or build from source:

```sh
go install github.com/mjpowersjr/claude-pack@latest
```

## Usage

```sh
# Bundle the current directory + its Claude sessions/memories
claude-pack export

# Bundle a specific directory, skipping heavy folders
claude-pack export --dir ~/code/myproj --output myproj.claude.tgz --exclude node_modules --exclude .git

# Bundle ONLY the Claude sessions/memories (no project files) —
# handy when the code already travels via git
claude-pack export --sessions-only --output myproj-sessions.tgz

# See what's inside an archive without touching anything
claude-pack inspect myproj.claude.tgz

# Restore on another machine — to any path you like
claude-pack import myproj.claude.tgz --dest ~/work/myproj
cd ~/work/myproj && claude --resume
```

Run `claude-pack` with no arguments in a terminal for a guided **interactive mode**, and `claude-pack <command> --help` for all options.

### What gets bundled

| Archive path        | Contents                                              |
|---------------------|-------------------------------------------------------|
| `manifest.json`     | Original path, hostname, timestamps, file counts      |
| `project/`          | Your directory (files, subdirs, symlinks)             |
| `claude/sessions/`  | `~/.claude/projects/<encoded>/*.jsonl` session logs   |
| `claude/memory/`    | `~/.claude/projects/<encoded>/memory/` memory files   |

### Path relocation

On import, `claude-pack`:

1. Computes the new encoded project name for the destination path.
2. Rewrites the old absolute path → new path inside every session `.jsonl` and memory file (including JSON-escaped Windows paths and the encoded directory name itself).
3. Installs sessions/memories under `~/.claude/projects/<new-encoded>/`.

### Safety

- **Never clobbers by default.** Existing archives, project files, sessions, and memories are left alone; you get a clear warning and a `--force` escape hatch.
- Refuses to import into a non-empty destination directory without `--force`.
- Exports write to a temp file and rename on success — a failed export never leaves a truncated archive.
- Archive extraction rejects path-traversal entries (`../`, absolute paths).

### Options you might need

- `--claude-dir PATH` — use a non-default Claude config dir (also respects `$CLAUDE_CONFIG_DIR`).
- `--sessions-only` — on export, bundle only the Claude sessions/memories; on import, restore only those (alias: `--skip-project`). Sessions-only archives are detected automatically on import, and `--dest` names the project directory the sessions should attach to (it is left untouched).

## Development

```sh
go build -o claude-pack .   # build
go test ./...               # test
```

Pure Go standard library — no dependencies. Tagged releases (`v*`) automatically build and publish binaries for Linux, macOS, and Windows (amd64 + arm64) via GitHub Actions.

## License

[MIT](LICENSE)
