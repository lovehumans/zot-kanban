package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// gate.go is who may do what: the Mover that names which door asked, the Claim that says which session holds a
// Card, the Column gate every move and every close is asked of, and the gates the Agent's edit and step doors
// carry beside it. A door quotes these answers rather than holding its own copy of the rule.

// A Mover names who asked for a move. An Agent's move carries the name of the session it began with, the
// readable alias zot gives a persisted conversation and the hex SessionID only where there is no alias;
// a human move carries none, because neither the browser's POST nor a hand on the file has a session, and
// a human outranks a claim.
type Mover struct {
	Session string
	// Agent is the fact that the door asking is the Agent's tool and not a hand on the board. A session name
	// is not that fact: a tool process can know no session, and the words a refusal answers in depend on the
	// door that asked, not on whether it managed to name who holds the Card.
	Agent bool
	// Above is the place a drag named: the id of the Card the dragged Card lands above, or PlaceEnd for
	// after the Column's last Card. Empty names no place, which is what a tool move and a drop onto a
	// Column's heading are.
	Above string
}

// HumanMove is what a drag on the board passes: nothing is claimed by it, and no claim refuses it.
var HumanMove = Mover{}

// at names the place a drag's dotted line pointed at on the mover that made the drag.
func (m Mover) at(above string) Mover {
	m.Above = strings.TrimSpace(above)
	return m
}

// AgentMove is what the tool passes: the Card is claimed for, and checked against, this session.
func AgentMove(session string) Mover { return Mover{Session: strings.TrimSpace(session), Agent: true} }

// cardClaim is the Claim a Card's file carries, nil for a Card nobody holds, and a refusal for a file that is
// not JSON at all. It is read off the raw bytes because the claim gate runs before a Card is decoded into its
// struct, and a file the gate cannot read is a Card the gate will not move: reading it as "unclaimed" would
// let a hand-broken Card through the gate that exists to refuse it.
func cardClaim(raw []byte) (*Claim, error) {
	var held struct {
		Claim *Claim `json:"claim"`
	}
	if err := json.Unmarshal(raw, &held); err != nil {
		return nil, fmt.Errorf("card file is not a JSON object: %w", err)
	}
	if held.Claim == nil || held.Claim.Session == "" {
		return nil, nil
	}
	return held.Claim, nil
}

// claimGate is the claim gate, read off the Card's own file before anything is renamed: a Card one session
// holds is not moved by another, whatever Column that session names - including the Column it is already
// sitting in, so asking for doing is not a way around a claim. A move with no session behind it is a human's,
// and a human outranks a claim. It answers the Claim the Card carries - nil when nobody holds it, which is
// also the answer for a human's move, whose gate never had to read one - and the refusal it decided, so a verb
// that takes a Claim on needs no second reading of the file to know whether the Card was held.
func claimGate(raw []byte, cardID string, by Mover) (*Claim, error) {
	if by.Session == "" {
		return nil, nil
	}
	held, err := cardClaim(raw)
	if err != nil {
		return nil, err
	}
	if held == nil || held.Session == by.Session {
		return held, nil // a Card nobody holds is nobody's to refuse
	}
	since := ""
	if !held.At.IsZero() {
		since = " since " + held.At.Format(time.RFC3339)
	}
	return nil, fmt.Errorf("%w: card %s is held by session %s%s - only that session moves it, and a drag out of doing on the board releases it",
		ErrClaimed, cardID, held.Session, since)
}

// The Column gate is the Board's answer to the question every door asks: may this door carry a Card from
// there to here. Move consults it before anything is renamed and Close before anything leaves the board, and
// the web door quotes its answers instead of holding its own copy of the rule, which is what a new Column
// rule now costs: one line here. Its words carry no ADR citation: an install ships this source and web/, and
// not the documents a citation would name. Where the two readers need different words the answer carries an
// addendum for the Agent, who is being told what to send, and not for a hand dragging a Card, which has
// nothing to send.
const (
	// ruleIntoDone and ruleIntoClosed are what a Column nothing lands in prints under its own name, and what
	// refuses a move there: done is entered over a commit and closed over a close, and no drag or tool is either.
	ruleIntoDone      = "only a commit moves a Card into done"
	ruleIntoClosed    = "only a close puts a Card into closed"
	ruleIntoDoneAgent = " - name the commit that carries the change"
	// The closed addendum names the door that can, because the mover being refused is holding no handle.
	ruleIntoClosedAgent = " - closing a done Card is the modal's button or zot-kanban close, and both are the human's"
	// ruleCloseFrom is why Close answers only a Card in done.
	ruleCloseFrom = "close is the door a done Card has"
	// ruleCloseSteps is why Close answers a Card in done that is still open on one of its own steps: closing keeps
	// a Card as finished work, and a step the Card wrote and never marked says the work it named is not over.
	// The refusal says the open steps before it, so the words a human acts on - the box to tick - are
	// already on the line.
	ruleCloseSteps = "close it once no step on it is open"
	// ruleOutDone and ruleOutClosed are what a Card is answered with when a mover asks for it out of a Column
	// nothing lands in: unfinished work is a Card of its own, and closed is reopened rather than moved.
	ruleOutDone   = "a Card in done is done - unfinished work gets a Card of its own, and a wrong done is a commit to undo"
	ruleOutClosed = "a Card in closed is reopened, not moved - zot-kanban reopen is the door out of it"
)

// outRules are the Columns no door carries a Card out of, with the words that say what does. Move refuses a
// move out of any Column named here, so the Agent's door no longer un-does a Card a hand may not even lift,
// and the web door refuses a hand the same way. What is marked done is done: unfinished work gets a Card of
// its own, and a done that turns out wrong is a commit to undo or a file a human hand edits.
var outRules = map[string]string{
	DoneColumn:   ruleOutDone,
	ClosedColumn: ruleOutClosed,
}

// ruleFor is the words a Column prints under its own name, and the fact that marks it `data-no-drop` in
// web/column.fragment.html at all; an empty answer is a Column a drag may land in. web/board.js says the same
// sentence while a drag sits over such a Column, because a Column that never accepts a drop never gets a
// request to refuse and the rule would be enforced in silence.
func ruleFor(col string) string {
	switch col {
	case DoneColumn:
		return ruleIntoDone
	case ClosedColumn:
		return ruleIntoClosed
	}
	return ""
}

// movable is the Card a hand may lift out of the Column it sits in: every Card except one in a Column no door
// carries a Card out of. The same fact decides whether a Claim is worn as an edge, because a held Card is one
// a human lets go of by dragging it.
func movable(c Card) bool {
	_, pinned := outRules[c.Column]
	return !pinned
}

// doorWords is the answer in the words this door's reader can act on: a mover at the Agent's tool is told what
// to send to make the move legal, and a human drag is not told something it cannot send.
func doorWords(by Mover, human, addendum string) string {
	if by.Agent {
		return human + addendum
	}
	return human
}

// columnGate is the gate asked of the Board. An empty refusal is a pair of Columns this door carries a Card
// between; anything else is said before the Card's file is touched, whatever door asked. The Column the mover
// asked for answers first - it is the one the mover chose - and the Column the Card sits in answers second.
func columnGate(cardID, from, to, commit string, by Mover) error {
	switch {
	case by.Agent && by.Session == "":
		// An Agent's move is made as a Session, and a mover that cannot name one has no legal move here: into
		// `doing` it would file a Card nobody holds, and out of it it would let go of a Claim it does not own,
		// because claimGate reads a mover with no name as a human's, and a human outranks a claim.
		// zot hands the name in `session_start` and replays it after a reload.
		return badInput("card %s: %w, and an Agent's move is made as a session; that Card sits in %s", cardID, ErrNoSession, from)
	case to == ClosedColumn:
		// closed answers a Close and nothing else. It is named here rather than left to validColumn so that the
		// answer is the rule and not a name this program does not know, and it is bad input rather than a
		// conflict: no re-reading of the board makes a move into closed right, because the door there is a close.
		return badInput("card %s: %s", cardID, doorWords(by, ruleIntoClosed, ruleIntoClosedAgent))
	case !validColumn(to):
		return badInput("unknown column %q (want one of %s)", to, strings.Join(Columns, ", "))
	case to == DoneColumn && commit == "":
		return badInput("card %s: %s", cardID, doorWords(by, ruleIntoDone, ruleIntoDoneAgent))
	}
	if why, pinned := outRules[from]; pinned {
		return conflict("card %s: %s", cardID, why)
	}
	return nil
}

// The Agent's edit door has two refusals a hand's does not. They are not part of the Column gate, which answers
// a different question - may this door carry a Card from there to here - and they carry no ADR citation, because
// a refusal is read by an Agent and an install ships no ADRs.
const (
	// ruleEditClosed names the doors that do have a closed Card: zot-kanban reopen, and the modal behind it.
	ruleEditClosed = "a Card in closed is not the Agent's to edit - reopening it is a hand's, and so is editing it"
	// ruleEditOrigin names the door an Agent does have onto a Card a hand captured: the Comment.
	ruleEditOrigin = "a Card a hand filed is the hand's to reword - say what you found in a Comment on it"
)

// agentEditGate is asked of the Agent's edit door before a Card's words are rewritten, inside the same locked
// window the write lands in. A Card whose origin nothing recorded - one filed before origin existed -
// is not known to be a hand's, and this gate lets it through.
func agentEditGate(cardID, col string, raw []byte) error {
	if col == ClosedColumn {
		return conflict("card %s: %s", cardID, ruleEditClosed)
	}
	fields, err := decodeCard(raw)
	if err != nil {
		return fmt.Errorf("card %s: %w", cardID, err)
	}
	if origin, _ := fields["origin"].(string); origin == byHuman {
		return conflict("card %s: %s", cardID, ruleEditOrigin)
	}
	return nil
}

// The step door's limits, and the refusals that name the doors a Card does have. Like the two an Agent's edit
// door carries, these sentences reach an Agent and an install ships no ADRs, so they say what to send rather
// than where the rule comes from.
const (
	// stepNameLimit is how long a Step's name may be, counted in runes: long enough to name one small task in
	// one line, short enough that a Card whose steps need a paragraph was two Cards.
	stepNameLimit = 42
	// maxLiveSteps is how many steps a Card holds at once. The cap is what keeps a Step one small piece of
	// work rather than a plan, and it is counted on the steps a Card holds, not on the numbers it has issued.
	maxLiveSteps = 7

	// stepUndo and stepErase are the two things a named ordinal may be asked to do besides being marked done.
	// erase is this door's word for taking a step off a Card: delete stays the verb for taking a Card off the
	// board, and the two are different things to take away. The tool's op, the modal's glyph and the word the
	// ladder prints under a spent ordinal all say erase.
	stepUndo  = "undo"
	stepErase = "erase"

	// ruleStepDoing names the Column a Card's steps are worked from, and the move that puts a Card there.
	ruleStepDoing = `a Card's steps are worked in doing: move {card, "doing"} takes the Card up and claims it for this session`
	// ruleStepClaim names what a Card sitting in doing is missing when this session holds no Claim on it.
	ruleStepClaim = `a Card's steps are written by the session holding it: move {card, "doing"} claims this one for you`
	// ruleStepClosed names the doors a Card in closed does have, as ruleEditClosed names them for the edit door. It
	// is asked of both step doors, because closed is the one Column no step verb reaches at all.
	ruleStepClosed = `a Card in closed answers no step from any door - reopening it is a hand's, and only then is it a Card that can be stepped on`
	// ruleStepHeld is the refusal the hand's door carries and the Agent's does not: the Claim that lets a session
	// write a held Card's steps is what shuts a browser's boxes on that same Card.
	ruleStepHeld = "a Card a session is holding is not a browser's to step on - drag it out of doing first, and the Claim goes with it"
	// ruleStepDone is said instead of ruleStepDoing for a Card already filed there, because the move that rule
	// names is no door a done Card goes through.
	ruleStepDone = `a Card filed done is not stepped - a hand reopens it, or writes its file`
	// ruleStepReword is the boundary a Step keeps from a Comment: its name is filed, and never reworded.
	ruleStepReword = `a Step's name is not reworded: name "text" for a new step, or "ordinal" alone for the step to mark`
)

// agentStepGate is asked of the Agent's step door before a Card's steps are written, and reads the Claim rather
// than the Origin: a Card a human filed is still the Card an Agent is working, and its steps are that session's
// to say. Every refusal here is a conflict: a Card in done is one because it sits in a Column this
// door does not answer, which is what a delete asked of finished work answers too, and a Card outside doing or
// held by another session is one because the door that would make it right - move - exists and this session has
// not asked it. closed is refused above it, in the door both callers share.
func agentStepGate(cardID, col string, raw []byte, session string) error {
	if strings.TrimSpace(session) == "" {
		return fmt.Errorf("card %s: %w, and an Agent's step names the session that wrote it", cardID, ErrNoSession)
	}
	if col == DoneColumn {
		// A conflict rather than bad input, the way a delete asked of a done Card answers: the request
		// is well-formed and it is the Column the Card sits in that refuses it. It names no move either way, since
		// no door carries a Card back out of done for an Agent to step it from.
		return conflict("card %s: %s", cardID, ruleStepDone)
	}
	held, err := cardClaim(raw)
	if err != nil {
		return fmt.Errorf("card %s: %w", cardID, err)
	}
	if col != DoingColumn {
		return conflict("card %s is in %s: %s", cardID, col, ruleStepDoing)
	}
	if held == nil {
		return conflict("card %s: %s", cardID, ruleStepClaim)
	}
	if held.Session != session {
		return conflict("card %s is held by session %s%s - only that session writes its steps", cardID, held.Session, claimWords(held))
	}
	return nil
}

// handStepGate is asked of the step door a hand reaches - the modal's box, and the shell if a step verb is ever
// added to it - and it carries the one refusal the Agent's gate does not: a Card a session is holding in doing is
// not a Card a browser steps on. The Claim is the hinge in both directions; the Agent writes a held Card's steps
// because it is the session working it, and the human gets the boxes back on that Card by dragging it out of
// doing, which is what releases the Claim. A hand outranks a Claim everywhere else on this board - a
// drag, an edit, a Comment - and a step is the one place it does not, because a step is a word about the work in
// progress rather than a change to the Card's words.
//
// It reads the Column before the Claim, because a Claim in done only records who did the work: a done Card keeps
// its boxes whatever its file's claim line says.
func handStepGate(cardID, col string, raw []byte) error {
	if col != DoingColumn {
		return nil
	}
	held, err := cardClaim(raw)
	if err != nil {
		return fmt.Errorf("card %s: %w", cardID, err)
	}
	if held != nil {
		return conflict("card %s is held by session %s%s: %s", cardID, held.Session, claimWords(held), ruleStepHeld)
	}
	return nil
}

// claimWords is when a Claim was taken, said the way both step doors say it. An empty answer is a Claim with no
// timestamp on it - a hand wrote that field into the file - and a refusal that guesses a time is worse than one
// that says nothing about it.
func claimWords(held *Claim) string {
	if held == nil || held.At.IsZero() {
		return ""
	}
	return " since " + held.At.Format(time.RFC3339)
}
