# zot-kanban

A Kanban board for organising agent work, delivered as a [zot](https://github.com/patriceckhart/zot) extension.

The board lives in the project it organises. Every Card is one JSON file under `.kanban/`, and the directory it sits
in is its Column. Git tracks the files, an agent files and moves them through its `kanban` tool, you triage them and
drag them in a browser, and the page is served by the extension's own process. No database, no hosted service, no
account.

```
<project>/.kanban/
  backlog/  inbox/  ready/  doing/  blocked/  done/      # a Card's Column is the directory holding it
  closed/                                                # finished work kept out of the board's face
  url   unseen   unseen_comments                         # the board's live state; not git-tracked
```

## What it is for

An agent working in your repository needs to know what has been agreed, what it is holding right now, and what is
finished. A chat session loses that. A ticket system outside the repository is a second place to keep it, and the
agent cannot read it. zot-kanban keeps it in the tree, where a Card is reviewed in a diff, undone with a revert, and
readable by every hand - yours, the agent's, `git`, your editor.

## The Columns

| Column | What it holds |
|---|---|
| `backlog` | ideas, kept out of the daily view |
| `inbox` | Cards filed, waiting for you to triage |
| `ready` | triaged: what an agent may take |
| `doing` | Cards an agent is working, each naming the session holding it |
| `blocked` | Cards that cannot move; the reason is a Comment on the Card |
| `done` | finished work, each Card naming the commit that carries it |
| `closed` | done work put out of the board's sight but kept |

The path one Card takes:

1. an agent files it - it lands in `inbox`, marked new until you open it;
2. you triage it in the browser - into `ready`, or `backlog`, or delete it;
3. an agent takes it from `ready` into `doing` - that move is its claim on the work, and no second session can take the same Card;
4. the agent commits the change;
5. the agent moves the Card into `done`, naming that commit's sha - the board checks the sha exists;
6. you `close` it when `done` gets long.

Two commits close one piece of work, and that is the design: the commit that carries the change, then the commit of
the Card that records it. `done` is reached only by a commit, and `closed` only by a close.

## Features

- **Plain files, in git.** A Card is a JSON file; its whole history, and a diff of every move, is git's. Read it with
  `cat`, edit it with your editor, move it with `git mv`.
- **The agent works through a tool, not through your files.** It files, edits, comments, reads and moves Cards with
  the `kanban` tool, and a refusal tells it - and you - why.
- **Claims.** A Card in `doing` names the session holding it and wears an orange edge on the board. Two sessions
  cannot work the same Card, and a drag by your hand is what ends a claim.
- **Done costs evidence.** Moving a Card into `done` names a commit, and the board refuses a name this repository
  does not have. A `done` Card whose commit is missing is shown as the contradiction it is.
- **Comments on the Card.** Discussion travels in the Card file, with author and time, so a Card you open a week
  later still says why it was blocked.
- **Marks for what arrived since you looked.** An agent-filed Card wears an asterisk, and an agent Comment paints the
  Card's comment count blue, until you open the Card.
- **A board you actually use.** Drag a Card onto the dotted line a drag draws, in a Column or across two. Click one
  to open it, edit its title, body and priority there, copy its id with a click. Type in the filter box (or press
  `/`) to narrow the board. Pick light, dark, or follow your browser.
- **It also works without zot.** `zot-kanban serve` serves this project's board in the foreground, for a triage pass
  in the morning.
- **Nothing is destroyed.** A delete moves the file to `.kanban/trash/`; a close moves it to `.kanban/closed/`. A
  deleted Card comes back with `zot-kanban restore`.
- **One binary, no build step.** The HTML, the stylesheet, the scripts and htmx are embedded in the Go binary. No
  npm, no CDN.

## Not on the menu

Anti-features, said outright, because knowing where this board stops is what makes it usable:

- **Not an issue tracker.** No GitHub or Jira import, no sync, no labels, assignees, due dates, estimates or
  sprints. A Card has a title, a body, a priority (`high` / `mid` / `low`), an order and Comments.
- **Not a hosted service.** It binds `127.0.0.1` and serves one project's board. There are no accounts and no auth,
  because there is nothing remote; two people share a board through git, not through logins.
- **No automation.** No CI hook, no auto-move, no "mark done when the PR merges". Nothing moves a Card that a hand
  in the browser or a tool call did not move.
- **Not the agent's board to clean.** The `kanban` tool has no delete and no close, and no move reaches `done`
  without a commit. Deleting and closing are held on the human's side, and so is rewording a Card you captured by
  hand: an agent's `edit` answers a Card its own tool filed.
- **No notifications.** No email, no push, no webhook. The marks on the board and the lines in zot's tool log are the
  whole signal.
- **Not a runner or a scheduler.** It records work; it does not execute it.
- **Not a database.** The board filters and counts. Anything beyond that is `git log -- .kanban/` or your own script
  over the JSON files, which is deliberately easy because they are just files.
- **No plugin system, no theme packages, no config file** beyond the extension manifest and one environment variable.

## Using it

### In zot — what an agent is handed

- `/kanban` — the Card count per Column, and the board URL and version that are actually answering.
- the `kanban` tool — five actions. `add` files a Card into `inbox` (`title`, `body`, `priority`). `edit` changes
  what one Card says where it sits - `title`, `body`, `priority`, and only the ones it names - and answers a Card its
  own tool filed, never one you captured by hand, nor one kept in `closed`. `read` lists the
  Columns a session works - `inbox`, `ready`, `doing`, `blocked` (`backlog`, `done` and `closed` only when you name
  them), or names one Card by its id and answers it in full, Comments included. `comment` files a Comment on a Card,
  or corrects one the agent's own session wrote. `move` carries a Card to another Column, and needs `commit` - the
  sha of the commit that carries the change - to move into `done`.
  - An agent takes a Card from `ready`, never from `inbox`: `inbox` is yours to triage, and when `ready` is empty
    what to take next is your decision.
  - An agent works a Card only while it holds it in `doing`; taking a Card on is the move that claims it.
  - A session that stops without finishing moves the Card back to where it came from.
- the **tool log** above your input while the tool works: one line per thing it did - a move, a filed Comment - each
  naming the Card and quoting none of it.
- a startup line with the board address, which the board also hands to your browser.

The rules an agent is bound by are published in the tool description zot hands it, so they are the same words the
refusals quote. What is above is their shape for a human, and the board page's **?** modal prints the published words
themselves.

### In the browser — what you have

`http://127.0.0.1:7498/` (it walks upward if that port is held, and reports the address it bound; `/kanban` and
`zot-kanban url` both tell you). The page re-reads the board every 3 seconds, so an agent's change shows up on its
own.

- **Drag** a Card onto the dotted line it draws - between two Cards, above the first, or past the last - to place it
  there, in a Column or across two. `done` and `closed` draw no line and say why: only a commit reaches `done`, only
  a close reaches `closed`.
- **Click** a Card to open it. That view is where you read its body, Comments and commit and where you edit its
  title, body and priority; the words at the top of it are the field that edits the title. Its id sits in the meta
  line as the button that copies it, so naming a Card to an agent is a paste. A Comment long enough to run past 10
  lines shows 10 and a **…**, and opens the whole of it when you click; click again to fold it.
  Its body and Comments are read as the Markdown their file holds - headings, flat lists, fenced code, `code`,
  **bold**, *italic* and links - and clicking that rendered body brings the editable field back. Where you are typing,
  the field still shows the Markdown you are writing, and a Card read outside the browser is the same plain
  text. **Esc** or a click beside that view closes it, and either one asks first when you have typed words that
  are not saved; the **✕** at its top right closes it without asking, because it already says it changes
  nothing.
- **New Card** at the top files one into `inbox`.
- **Ctrl+S** saves the Card open in the modal, and **Ctrl+Enter** files what you are typing - the new Card in the
  strip, or the Comment in the modal. Both keys write the form your caret is sitting in; `Cmd` answers as `Ctrl`
  does, and Ctrl+S is left to the browser where no form is in reach. The three submit controls name their key in
  their tooltip.
- The box that says **/ to filter cards** narrows the board to Cards whose title, id, body or Comments carry every
  word you type.
- The **theme** select: `auto` follows your browser's light or dark preference, and your own choice beats it. The
  choice lives in that browser.
- **Marks.** An asterisk before a title is an agent Card nobody has opened; a blue comment count is an agent Comment
  nobody has read; a thick orange edge is a Card an agent is holding. Opening the Card clears the first two, and only
  your drag clears the third.
- **Footer buttons** on an open Card: **delete** for a Card in a working Column (it asks first, and moves the file to
  `.kanban/trash/`), **close** for a Card in `done`, **reopen** for a Card in `closed`. The footer's `closed N` is the
  link that draws the closed Cards as a seventh Column.
- **?** at the header's right end opens the board's own explanation of its workflow — the Columns, the path one Card
  takes, the actions you have and the agent's, and the rules an agent is bound by, quoted from the text the tool
  publishes and folded shut until you open them. It is part of the page: opening it asks nothing of the server.

### From the shell

```bash
zot-kanban serve [--port N]        # this project's board without a zot session; a port you name is bound exactly
zot-kanban url                     # the address of the board answering right now
zot-kanban delete <id>             # a Card off the board: its file moves to .kanban/trash/ (never a done or closed one)
zot-kanban restore <id> [col]      # put a deleted Card back, into inbox when no Column is named
zot-kanban close <id>              # keep a done Card out of the board's face: its file moves to .kanban/closed/
zot-kanban close --all-done        # close every done Card whose commit evidence holds up
zot-kanban reopen <id>             # bring a closed Card back into done
zot-kanban version
```

A board start opens it in your browser (`xdg-open`, `open`, `cmd /c start`). `ZOT_KANBAN_NO_OPEN=1` keeps it in the
terminal - the only lever a zot session gives you, since a session starts its extension with no flags.

### By hand, or with git

A Card is `.kanban/<column>/<id>.json`, and the id - `<UTC time>-<title slug>`, like
`2026-11-02T2225-wire-the-claim-field.json` - is the filename stem and never changes when the Card moves. The slug
is what you read on the board; the whole id is what the tool and the shell commands name.

```bash
git mv .kanban/inbox/2026-11-02T2225-wire-the-claim-field.json .kanban/ready/
```

```json
{
  "body": "…",
  "claim": {"at": "2026-11-03T12:00:00Z", "session": "tidy-birch"},
  "comments": [{"id": 1, "at": "2026-11-03T09:00:00Z", "by": "agent", "session": "tidy-birch", "text": "why it is blocked"}],
  "commit": "1a2b3c4",
  "created": "2026-11-02T21:25:00Z",
  "order": 50,
  "origin": "agent",
  "priority": "high",
  "title": "Wire the claim field",
  "updated": "2026-11-03T09:00:00Z"
}
```

Only `title` is required; `claim` belongs to a Card in `doing` and `commit` to one in `done`, and the board writes the
object indented with two spaces and with its keys in name order, so an edit shows in git as the lines it names. A
dated note you type into `comments` with nothing else is a valid Comment. A blocked Card carries its reason as a Comment - there is no
blocked-reason field. The board re-reads the files on every request, so a change made outside it shows up on the next
poll.

## Installing

zot reads `<install dir>/extension.json` and runs no build step, so what is installed has to be runnable as declared.
This repo declares `{"name": "kanban", "exec": "go", "args": ["run", "."]}` - the manifest name is the short one, so
`zot ext list` says `kanban` while the install directory keeps the project's name.

Extensions live in `$ZOT_HOME/extensions/<name>/` (`~/.local/state/zot/extensions/`), or project-locally in
`./.zot/extensions/`.

```bash
git clone https://github.com/lovehumans/zot-kanban   # the published copy: sources only, runnable as declared
zot ext install /path/to/zot-kanban   # copies the directory in as it is, `.git` included
just install-global                   # the same, staged so only the runnable files travel, over any global copy
zot ext list                          # name, version, enabled, dir
zot ext doctor                        # start it, and report its commands and tools
```

`zot ext doctor` is the check that an install is real: it launches the extension and reports `/kanban (active)` and
`kanban (active)`, or the reason it did not. `zot ext logs kanban -f` tails the extension's stderr
(`~/.local/state/zot/logs/ext-kanban.log`). `zot ext remove kanban` removes the installed copy; with both a global
and a project-local copy installed, zot refuses to guess which one you meant.

To run a prebuilt binary instead of recompiling at each zot start, change `exec` to `"./zot-kanban"`, `args` to `[]`,
and keep it built.

## Changes

[`CHANGELOG.md`](CHANGELOG.md) — every version and what the board began doing in it. The version in
`extension.json` is the version that file names, and every version gets a section there.

## License

[The Unlicense](UNLICENSE) - public domain. `UNLICENSE` holds the text and `LICENSE` is a symlink to it. The vendored
frontend, htmx 4.0.1, is 0BSD and its notice travels beside it in `web/htmx.js.LICENSE`.

The recipes in the `justfile` are the board's own commands, and `just --list` says what each one is for. The
mechanics behind the rules above - the development loop, the tests, the board's HTTP surface - belong to the
repository this extension is developed in, not to an installed copy of it.
