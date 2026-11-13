# Changelog

The version `extension.json` names is the version this file names, and every version gets a
`## v<version>` section here, newest first. A section says what a board does now, not which files
moved. This chronicle begins at 0.60.0; what came before it is named only by the version numbers
those bumps left behind.

## v0.70.0 — a hand files a step and erases one in a Card view, and its ladder shows what it spent

- The steps region holds a field: write a name under the ladder and press **Step** - or Ctrl+Enter - to file a step
  on the Card. It arrives unticked with its erase glyph ready, filing the same name twice is the step it already is,
  and a Card holding no steps still shows the field, because the field is how a Card gets its first rung. The name is
  at most 42 characters, which the field itself refuses at the keyboard.
- Each rung wears a trash glyph. Pressing it erases that step, after asking, and the ladder redraws with its rung
  saying *erased* - no box, no glyph, nothing to click: the name is gone from the file and the way back to it is git.
  The number that rung wore stays spent, so a number you read is never one a later step is handed. The count, the bar
  on the face and the seven-step limit keep counting the steps a Card holds rather than the ones it spent.
- The verb is *erase*. The tool's `step` action opens a step with `op:"undo"` and erases one with `op:"erase"` (was
  `op:"delete"`), and `delete` stays the word for taking a Card off the board.
- A mark now answers only its own rung, so ticking a box no longer redraws the field you are typing into; a filing
  and an erase answer the whole steps region, and an erase does clear a name typed but not yet filed. The field joins
  the fields a Card view asks about before it closes.
- A Card already holding seven steps has its field and its button drawn shut, `7 steps max.` written in their
  tooltips. Where an agent holds the Card, field, button, boxes and glyphs are all shut, and a drag out of `doing`
  opens them. Erasing the last open step of a Card in `done` answers what its **close** refused.
- A Card's id cuts its readable half at 42 characters, the length a step name is capped at, and drops a hyphen left
  standing at the cut; the cut may fall inside a word. The stamp and the whole id still name the Card, so a Card filed
  from a long sentence leaves a filename and a `read` argument a hand can take in at a glance.
- Where a ladder is shut - a Card a session is holding, a Card kept in `closed` - the reason under it is drawn as a
  notice: framed in the palette's quiet blue and wearing an info glyph, so it reads as the board telling you
  something rather than as another line of the Card's text. Its words keep the ink a Card is written in.
- A Card's Comment count is a number beside a bubble, with no word: the bubble is Tabler's plain `message`, the same
  outline the **add comment #4** button wears with its plus. The words the face dropped live on in the count's
  tooltip - `3 Comments on this Card`, or the sentence naming Comments nobody has read - and in the bubble's own
  name, so a reader who never rests a pointer still hears what the number counts. An unread count keeps the blue it
  already had, and the bubble takes it along with the number.

## v0.69.0 — a Card's steps are ticked in its modal, and a done Card short of one will not close

- A Card view lists its steps as checkboxes. Ticking one marks it done; unticking one opens it again. The number a
  click means is written on the box itself, so the wrong box cannot be clicked for the wrong step, and the list
  redraws where it was clicked: the modal stays open, and the count out on the board follows on the next poll.
- The boxes are gated on the Claim, as the tool's `step` door has always been. A Card a session is holding in
  `doing` is stepped from its claim, so its boxes arrive already ticked and shut, with the sentence that door would
  refuse a click with printed under them; drag the Card out of `doing` to step on it. A Card in `done` with a claim
  is stepped from that claim, and a Card kept in `closed` answers no step from either door.
- **close** now refuses a Card in `done` that still has a step open, naming the ordinals and quoting their names,
  and says to mark each one or drag the Card back off `done`. Moving a Card into `done` does not refuse it: the
  finding is painted on its face, which is where an Agent meets it. `close --all-done` skips such a Card and prints
  the finding rather than sweeping it away.
- A Card that carries steps now wears a bar under its meta line, one rung per step in ordinal order, filled for each
  one done: how far through is seen at a glance, `2/3` read when you want the number. A rung of a step left open on
  a Card in `done` or `closed` wears the unmet red. A Card with no steps wears no bar.

## v0.68.0 — a Card's steps are worked at the Claim, and its face says how far they got

- A `step` action joins the tool's six. It files a step on a Card you hold in `doing` (`card`, `text`), marks one
  done (`card`, `ordinal`), opens one again with `op:"undo"`, or erases one with `op:"delete"`. The steps belong to
  the Session holding the Card, whoever filed the Card; a Card in `done` or `closed` is not stepped, and no step is
  reworded.
- A step's number only grows. The ordinal you read is the step it names, and deleting the highest step does not put
  its number back on the shelf for the next one.
- A `read` says a Card's steps: a listing carries `2/3 steps` beside the Card, and one Card read whole lists every
  step with its ordinal, marked or open. Filing the same step twice, or marking one already marked, answers "no
  change" and writes nothing.
- A Card with steps prints how far through them it is on its face as `2/3`, and its Card view lists them; a Card with
  no steps prints nothing, and the `/` filter reaches a step name as part of the Card that files it.
- A Card filed `done` still showing a step undone wears the red a contradicted Card wears, and stays in `done`:
  the board paints an unmet step rather than refusing a finished Card for one.
- A Card's own file stays the human's door onto its steps; `normalize` repairs only a step's number there, and a
  hand may write more steps or longer names than the tool's door would file.

## v0.67.2 - Readme bugfixes
- Fixed a link

## v0.67.1 - Human readme
- Humanized the README.md by hand, since nobody likes reading a clanker's wall of text.

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
