<div align="center">
  <img src="zot-kanban.jpeg" width="200" alt="zot-kanban">
</div>

# zot-kanban

A Kanban board for organising agent work, delivered as a [zot](https://github.com/patriceckhart/zot) extension.

The board lives in the project it organises. Every Card is one JSON file under `.kanban/`, and the directory it sits
in is its Column. Git tracks the files, an agent files and moves them through its `kanban` tool, you triage them and
drag them in a browser, and the page is served by the extension's own process. No database, no hosted service, no
account.

[Features](#features-and-anti-features) · [Requirements](#requirements) · [Usage](#usage) · [Install](#install) · [Changelog](CHANGELOG.md) · [Unlicense](UNLICENSE) 

## Purpose

See the state of work on any project, no matter how big or tiny, how recent or old it is. Have a backlog of cards, see what's done and what the agent is currently doing.  
Add cards while the agent is working.

This is not a general purpose kanban board, but an opinionated workflow for one human and one or more LLM agents.

## Features and Anti-Features

| ✅ We do | ❌ We deliberately don't |
|---|---|
| ✅ Plain JSON files in git — works air-gapped, no remote needed | ❌ No hosted service: no accounts, no auth, no db, no MCP |
| ✅ agent works through the `kanban` tool; refusals quote the rules | ❌ No Automation, no CI hook |
| ✅ one claimed card per zot session (multi agent mutex) | ❌  Plugins|
| ✅ `done` needs a commit SHA| ❌ Not a generalized kanban board |
| ✅ Comments  | ❌ No Notifications |
| ✅ Simple Markdown rendering | ❌ Issue-tracker machinery: no import or sync, labels, assignees, due dates, sprints |
| ✅ One Go binary, web assets embedded | ❌ no npm, no CDN, no build step necessary|

A Card carries a title, a body, a priority (`high` / `mid` / `low`), an order and Comments. The board serves one
project's board on your own machine, so two people share it through git, not through logins.

## Requirements

- go runtime
- zot

## Usage

## The Board

| Column | What it holds |
|---|---|
| `backlog` | ideas kept for later, kept out of the agent's daily view |
| `inbox` | Cards filed, waiting for you to triage |
| `ready` | triaged: what an agent may take |
| `doing` | Cards an agent is working, each naming the session holding it |
| `blocked` | Cards that cannot move; the reason is a Comment on the Card |
| `done` | finished work, each Card naming the commit that carries it |
| `closed` | done work put out of the board's sight but kept |

The path one Card takes:

1. a human or an agent files it - it lands in `inbox`;
2. you triage it in the browser - into `ready`, or `backlog`, or `delete` it;
3. you discuss with the agent, the agent takes notes in the Card's `comments`;
4. an agent takes it from `ready` into `doing` - that move claims the Card, and no second session can take the same Card;
5. the agent commits the change;
6. the agent moves the Card into `done`, naming that commit's SHA - the board checks the SHA exists;
7. you `close` it when `done` gets long.

Two commits close one piece of work, and that is the design: the commit that carries the change, then the commit of
the Card that records it. `done` is reached only by a commit, and `closed` only by a human action.

### In the browser

Default url is `http://127.0.0.1:7498/` (it walks upward if that port is held, and reports the address it bound; `/kanban` and
`zot-kanban url` both tell you). The page re-reads the board every 3 seconds, so an agent's change shows up on its
own.

- **Drag** a Card to move or sort.
- **Open** a Card. Read, edit, comment. 
- **Help** page shows details.
- **New Card** at the top files one into `inbox`.
- **Ctrl+S** to save changes. 
- **/ to filter cards** narrows the board to Cards whose title, id, body or comments contain every keyword.
- **theme** select: `auto`, `light` or `dark`.
- **delete** a Card in a working Column (it asks first, and moves the file to `.kanban/trash/`) 
- **close** a Card in `done`, **reopen** a Card in `closed`. 

### In the zot TUI

- a startup line with the board address, which the board also opens in your browser.
- `/kanban` — the Card count per Column and the board URL.
- the `kanban` agent tool — five actions. 
  - `add` files a Card into `inbox` (`title`, `body`, `priority`). 
  - `edit` changes what one Card says where it sits - `title`, `body`, `priority`, and only the ones it names - and answers a Card its own tool filed, never one you captured by hand, nor one kept in `closed`. 
  - `read` lists the Columns a session works - `inbox`, `ready`, `doing`, `blocked` (`backlog`, `done` and `closed` only when you name them), or names one Card by its id and answers it in full, Comments included. 
  - `comment` files a Comment on a Card, or corrects one the agent's own session wrote. `move` carries a Card to another Column, and needs 
  - `commit` - the SHA of the commit that carries the change - to move into `done`.
- the **tool log** above your input while the tool works: one line per thing it did - a move, a filed Comment - each naming the Card and quoting none of it. Stays until your turn.


The rules an agent is bound by are published in the tool description zot hands it, so they are the same words the refusals quote. 

### From the shell

In case you need it w/o wanting to run zot:

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

This is an opinionated tool giving your a workflow, however the cards are just files, you can do with them whatever you want.

A Card is `.kanban/<column>/<id>.json`, and the id - `<UTC time>-<title slug>`, like
`2026-11-02T2225-wire-the-claim-field.json` - is the filename stem and never changes when the Card moves. The whole id is what the tool and the shell commands name.

Move a card from inbox to ready:
```bash
git mv .kanban/inbox/2026-11-02T2225-wire-the-claim-field.json .kanban/ready/
```

Fields within a card:
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

```bash
zot ext install https://github.com/lovehumans/zot-kanban  
```

To run a prebuilt binary instead of recompiling at each zot start, change `exec` to `"./zot-kanban"`, `args` to `[]`, and build it with `go build -o zot-kanban .`.

### Uninstalling
~~~bash
 zot ext remove kanban
~~~
