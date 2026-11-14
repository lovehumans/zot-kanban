package main

import (
	"errors"
	"fmt"
	"strings"
)

// A refusal kind names what a Write that did not happen means, and it is named at the point the Board
// refuses it: the Board is the only module that knows whether a human could have asked differently. badInput
// is a request no wording of it would do, noCard is an id no Card file holds, conflict is the board not being in
// the state the request assumed, and boardFailure is the board failing at its own job - a file it cannot read,
// a write that did not land - which is also what a refusal no site named reads as.
//
// A kind changes what a refusal costs a door and never what it says: the sentence a human reads is the same
// one the tool and the shell print, byte for byte.
type Refusal int

const (
	RefusalBoardFailure Refusal = iota
	RefusalBadInput
	RefusalNoCard
	RefusalConflict
)

// refusalKindName is a kind's own word, so a test that got the wrong kind says which one it meant.
func refusalKindName(kind Refusal) string {
	switch kind {
	case RefusalBadInput:
		return "bad input"
	case RefusalNoCard:
		return "no Card with that id"
	case RefusalConflict:
		return "conflict"
	default:
		return "the board's own failure"
	}
}

// refusal is the error that carries a kind. It is what errors.As finds in a chain, so the Board's habit of
// dressing a refusal in its own words - "card %s: %w" - names nothing again: only the site that decides the
// refusal names its kind, and the kind travels out through whoever wraps it.
type refusal struct {
	kind Refusal
	err  error
}

func (r *refusal) Error() string { return r.err.Error() }
func (r *refusal) Unwrap() error { return r.err }

// badInput, noCard and conflict name a refusal at the site that decided it. What is left untagged is the board
// failing at its own job, which is the answer every read, write and rename failure wants.
func badInput(format string, args ...any) error {
	return &refusal{RefusalBadInput, fmt.Errorf(format, args...)}
}
func noCard(format string, args ...any) error {
	return &refusal{RefusalNoCard, fmt.Errorf(format, args...)}
}
func conflict(format string, args ...any) error {
	return &refusal{RefusalConflict, fmt.Errorf(format, args...)}
}

// namedRefusal is a sentinel that carries its kind, so the many sites that dress a sentinel in words name
// nothing again. It is still a sentinel: the same value sitting in a chain is what errors.Is reports.
func namedRefusal(kind Refusal, sentence string) error {
	return &refusal{kind, errors.New(sentence)}
}

// kindOf reads a refusal's kind out of its chain. The outermost named kind wins, because the site that
// decided the refusal is the outermost one that named one. What no site named is the board failing at its
// own job: a Board write that stopped for no named reason is that failure, and it is the answer
// for everything a refusal table does not name rather than letting it fall through as bad input.
func kindOf(err error) Refusal {
	var named *refusal
	if errors.As(err, &named) {
		return named.kind
	}
	return RefusalBoardFailure
}

// ErrStale names a write that lost its race: another writer moved the Card first.
var ErrStale = namedRefusal(RefusalConflict, "the Card moved while this write was in flight")

// ErrAmbiguousID names an id two Cards hold at once. Nothing that writes through an id can be trusted
// to mean one of them, so an id found twice is refused rather than resolved.
var ErrAmbiguousID = namedRefusal(RefusalConflict, "one id is held by two Cards")

// heldTwice is the twin refusal said once. The Board meets one id held by two Card files three ways - a walk
// that sights an id in two Columns, a lookup that finds it twice, and a link that lands on a name already
// taken - and answers every one with the id, the files that hold it in full, and the one thing a hand can do
// about them.
func heldTwice(id string, paths ...string) error {
	return fmt.Errorf("%w: %q is in %s - rename one of them",
		ErrAmbiguousID, id, strings.Join(paths, " and "))
}

// ErrNoCard names an id no Card file holds, in any Column. It is named so that a caller can tell "this id
// is not on the board" from "this board refused what the request asked for" without reading the sentence.
var ErrNoCard = namedRefusal(RefusalNoCard, "no Card with that id")

// ErrCommentTaken names a Comment ordinal that another Comment already holds. A writer names the ordinal
// its Comment means to take, so a submission that arrives twice cannot file twice, and a submission that
// finds the ordinal filled by different text is a conflict the writer has to see rather than a write that
// quietly takes the next number.
var ErrCommentTaken = namedRefusal(RefusalConflict, "that Comment id is already on this Card")

// ErrCommentOrdinal names a Comment ordinal no Comment holds because it is past the end of the Card: the next
// one this Card would issue is a lower number. A filing raises the next free ordinal, so a form that went stale
// finds its own number taken and is refused as ErrCommentTaken; this one needs the highest ordinal to have come
// down - a hand editing the file, a Card restored with fewer Comments - or a number a request simply wrote too
// high. It is named apart because ErrCommentTaken says a number is held, and here nothing holds it.
var ErrCommentOrdinal = namedRefusal(RefusalBadInput, "that Comment ordinal is past the end of this Card")

// ErrCommentSuperseded names a Comment an agent tried to correct that a hand has already answered: a
// Comment written by hand sits after it on the Card. The edit window is the agent's own word being the last
// one the Card carries; once a human has replied, what the agent has to say is a new Comment beside that
// answer, not a rewrite of the text the human read.
var ErrCommentSuperseded = namedRefusal(RefusalConflict, "that Comment is not the last word on this Card")

// ErrNameTaken names a Card write that found a file already sitting at the name it chose. A Card file is
// only ever replaced by a write that read it first, so this is a race won by somebody else and it is
// reported rather than papered over.
var ErrNameTaken = namedRefusal(RefusalConflict, "a Card already holds that name")

// ErrClaimed is a Card held by one session being asked of another. errors.Is reports it; the text names
// the session that holds it, so the agent being refused knows whose Card it tried to take.
var ErrClaimed = namedRefusal(RefusalConflict, "that Card is claimed by another session")

// ErrNoSession names an Agent door that cannot say which Session it is working as. A Claim names the Session
// that took a Card up and a Comment the Session that wrote it, so a door holding no name would write a fact
// about nobody: a Card filed in `doing` that no session holds, a Claim somebody else's released, and a Comment
// whose `"session": ""` matches another session-less process's Comment in the guard that compares one session
// against another. An Agent always has a Session, so this is the broken state named
// rather than a value to work around.
var ErrNoSession = namedRefusal(RefusalBadInput, "this process knows no session")

// ErrNoCommit is the refusal a done Card's sha gets when nothing in the repository answers to it. A done
// Card is reopened by reverting the commit it names, so a name no repository has is a Card that
// can never be reopened: the sha is checked rather than taken as given. It is the one sentinel
// with no kind of its own, because its two refusals are different ones: a name that is not the shape of a
// sha is bad input, and a repository that answers nothing about a well-shaped name is a conflict.
var ErrNoCommit = errors.New("no commit with that name in this repository")
