# Changelog

The version `extension.json` names is the version this file names, and every version gets a
`## v<version>` section here, newest first. A section says what a board does now, not which files
moved. This chronicle begins at 0.60.0; what came before it is named only by the version numbers
those bumps left behind.

## v0.67.0 — the sheet is cut into a palette

- The board's styling is now two sheets: `web/gruvbox.css` holds the palette and what each colour means
  on this board, `web/board.css` holds the rules that wear it. Every colour the board paints with has
  one definition, so a palette can be swapped without rewriting a stripe.

## v0.66.5 — three more colours become roles

- An empty Column, a Column's count and the backlog's heading take their colour from a named role the
  palette binds, not from a hue spelled where they are painted.

## v0.66.4 — the switch's parts become roles

- A drag's drop target, a hovered save and high Priority's segment ink are roles too, so both themes
  bind them to the same bytes.

## v0.66.3 — Priority reads with its label beside the segments

- The Priority switch prints its label next to the segments rather than above them.

## v0.66.2 — a Card view holding unsaved words asks before it closes

- Closing a Card you have edited but not saved confirms first instead of discarding your words.

## v0.66.1 — the theme select wears a sun and a moon

- The dark/light choice is drawn as an icon rather than a word.

## v0.66.0 — a Card's words are read as Markdown in the browser

- The board renders a Card's body and Comments as Markdown; the Card file keeps the Markdown exactly as
  it was written, and the tool hands the same words over.

## v0.65.0 — a version change is noticed by the poll, not by a hard reload

- When the extension restarts on a new version, an open board page picks it up on its next poll.

## v0.64.0 — which edge a Priority stripe wears says who filed the Card

- A stripe on the left is yours, on the right an agent's.

## v0.63.1 — the footer prints no server clock

- The board stops printing a time it cannot promise is yours.

## v0.63.0 — a shortcut is named where you would click it

- The three submit controls name their Ctrl key in their tooltips.

## v0.62.0 — Ctrl+S saves a Card, Ctrl+Enter files one

- The keys that write a form write the form your caret is in.

## v0.61.0 — the tool's `edit` answers no Card a hand filed

- An agent can change only a Card its own tool filed; Cards you typed stay yours.

## v0.60.0 — the board explains its own workflow

- The header's `?` opens a modal holding the board's rules, so the page teaches itself.
