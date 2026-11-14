package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/agent/ext"
)

const version = "0.74.0" // must match extension.json, which is what zot ext list reports

// The command and the agent tool share a name, the way zot-review names both its command and its
// tool "review". The extension is named for what it is, without the project prefix, because inside a
// zot session that prefix says nothing the session does not already know. The repository,
// its CLI and the board's own name on the page keep "zot-kanban": zot dedups a project-local install
// against a global one by directory basename, so a renamed install directory would load two boards.
const (
	extName  = "kanban" // must match extension.json: it is how zot names us in ext list, ext remove and ext logs, and which ext-<name>.log this extension's stderr lands in
	cmdName  = "kanban"
	toolName = "kanban"
)

// boardRules is the board's rules in one text, published in one place: the tool description, which is the surface
// a model is looking at on every turn. What lives here is what no refusal can teach - a refusal already says what
// the Board refused, so the sha a done move costs, the fact that closed is no move target, and whose Claim blocks
// a move are said by the refusal that answers them, and saying them again here is how two copies of one rule drift
// apart. These are the rules a session has to hold before it calls anything: which Column a Card is taken from,
// when work has begun, what a session claims, how a claim is released when a session dies, and when a session may
// move a Card into ready on its own.
const boardRules = `Take a Card from ready, never from inbox: inbox is the human's triage column, so reading it is
seeing what waits, not taking it, and when ready is empty what to take is the human's to decide. A Card is worked
only from doing, held by you: taking a Card on - to grill it, plan it or build it - is the move into doing, and it
comes before reading source with the intent to change it, drafting files, coding or committing. Reading a listing,
or one Card to answer a question about the board, is not work. Claim every Card you have taken on; two
implementations at once only when they share code and cannot be done separately. A session that stops without
finishing moves the Card back to the Column it came from, which releases the claim; a Card left claimed by a
session that died is the human's drag to release. Work the human asks for in chat still gets a Card first, into
inbox for triage: move it into ready yourself only when the human permitted that in chat, and name the permission
in a Comment. A Card face in a Column names the Card by its title and prints no id, so an id is copied from a
Card's own view. Work a Card's steps as you go, mark one done the moment it is done, and read a Card you have not
looked at since before you touch it again.`

// toolActions is what this tool's six actions are and what each one takes, said once beside the rules they
// serve. Nothing is required at schema level: what an action needs is named by the refusal that answers a call
// missing it.
const toolActions = `add {title, body?, priority?} files one Card per unit of work into inbox, where the human triages.
edit {card, title?, body?, priority?} changes what a Card says where it sits, and changes only the fields it
names; it answers a Card filed by the tool, and refuses one a hand filed or one sitting in closed.
read {column?} lists the Columns a session works - inbox, ready, doing, blocked - each Card with its id, priority,
title, step count and Comment count, and the session holding it; backlog and done appear when named. read {card}
answers that one Card from whatever Column holds it - Column, file, body, steps, Comments in full, priority, order,
dates, commit and claim - because a copied id is a question about one Card; naming a Card and a Column together is
refused.
comment {card, text} files a note on a Card, including why it is blocked; comment {card, id, text} corrects that
Comment - your own from this session, and only while no Comment written by a hand follows it. step {card, text}
files one small task on a Card this session holds in doing; step {card, ordinal} marks one done, and
{card, ordinal, op:"undo"|"erase"} opens it again or erases it. move {card, column} carries a Card to another
Column by renaming its file; into done it carries commit, the sha of the commit that carries the change, and into
doing it claims the Card for this session.`

// toolFileRule is the rule about reaching a Card with a file tool, and it belongs to the tool rather than to
// help(): help is also what a human reads at a shell, and for a human the board is a directory tree to edit with
// anything they like.
const toolFileRule = `Use only these provided tools to interact with the Cards, and refrain from touching the board's files directly. If you need to touch them directly, ask the human for permission first.`

// toolDescription is the whole text a session reads before it calls the tool: what the tool is, what its actions
// take, the board's rules, and the rule about the files behind them. It is canonical for the rules - a tool
// description is in the model's context on every turn, while help() is only ever seen at a refusal or a shell.
func toolDescription() string {
	return "Discuss or record work on this project's kanban board: a Card is one unit of work, and the board is the " +
		"Columns a session works through. The tool takes nothing off the board and closes nothing - deleting and " +
		"closing are a human's doors. " + toolActions + " " + boardRules + " " + toolFileRule
}

// help is the tail of every tool refusal and what `/kanban` answers a hand that typed an argument. Its first line
// carries the version answering, because a refusal is read as authority: the incident it guards was a
// 0.31.0 install quoting the Comment rules that 0.34.0 had deleted, and nothing a session could read said which
// board was talking. The rules a session works to are not here - the tool description carries them - because a
// refusal is one sentence about one call and the Board has already said what it refused. The shell's own verbs are
// quoted from usageText rather than restated, so that a hand told to run one is told the argv that text says.
func help() string {
	return "kanban " + version + "   the board answering this call; the page footer and `zot-kanban version` print the same\n" +
		`/kanban                 this project's board counts and the board URL

the kanban tool, and what each action takes
  add     {"title", "body"?, "priority"?}
  edit    {"card", "title"?, "body"?, "priority"?}
  read    {"column"?} or {"card"}
  comment {"card", "text", "id"?}
  step    {"card", "text"} or {"card", "ordinal", "op"?}   // op is "undo" or "erase"
  move    {"card", "column", "commit"?}

What a tool action does not offer - closing, reopening, deleting, restoring - is a hand's, at the board or at the
shell:` + "\n" + usageText
}

// toolArgs is the kanban tool's arguments, kept narrow on purpose: one action
// discriminator and the fields that action needs, not one tool per verb.
type toolArgs struct {
	Action    string `json:"action"`
	CardID    string `json:"card"`
	Text      string `json:"text"`
	CommentID int    `json:"id"`
	// Op and StepOrdinal belong to the step door: the ordinal names one of a Card's own steps, and op says whether
	// that step is opened again or erased. A bare ordinal marks it done.
	Op          string `json:"op"`
	StepOrdinal int    `json:"ordinal"`
	// Title, Body and Priority are named-or-omitted rather than plain strings: an edit that names a title alone
	// must leave the body where it is, and a field typed as a string cannot tell "left out" from "set to nothing".
	Title    *string `json:"title"`
	Body     *string `json:"body"`
	Priority *string `json:"priority"`
	Column   string  `json:"column"`
	Commit   string  `json:"commit"`
}

// named is what an argument a call left out reads as where a door wants a plain string: the empty string, which
// is what Board.Add already gives its own defaults to.
func named(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// toolSchema names the six actions this tool has today. Nothing is marked required here: what an
// action needs is reported by the refusal that names the missing argument and the action. A field's description
// carries only the fact about that argument - the rules those arguments serve are said once, in the tool
// description - because this JSON is read by a model deciding what to type, and two copies of one rule are two
// chances to word them differently.
const toolSchema = `{
  "type": "object",
  "properties": {
    "action":   {"type": "string", "enum": ["add", "comment", "edit", "move", "read", "step"], "description": "the action this call asks for; add is the default; edit changes only what it names"},
    "title":    {"type": "string", "description": "add and edit: one line naming the work"},
    "body":     {"type": "string", "description": "add and edit: one or two sentences"},
    "priority": {"type": "string", "enum": ["high", "mid", "low"], "description": "add and edit: how the human triages it"},
    "card":     {"type": "string", "description": "the Card's id: comment, edit, move and step work on it; read names one Card instead of a Column"},
    "text":     {"type": "string", "description": "comment: the Comment to file; step: the name of the step to file"},
    "ordinal":  {"type": "integer", "description": "step: which of this Card's steps, as read {card} printed it; a bare ordinal marks it done"},
    "op":       {"type": "string", "description": "step: \"undo\" opens one already marked done, \"erase\" erases one; a step erased keeps its ordinal off the later ones"},
    "id":       {"type": "integer", "description": "comment: the ordinal of the Comment to correct, of this session, while no hand-written Comment follows it"},
    "column":   {"type": "string", "enum": ["backlog", "inbox", "ready", "doing", "blocked", "done", "closed"], "description": "move: the Column to carry the Card into - closed is not one of them; read: the Column to list, where an absent column means the four a session works: inbox, ready, doing, blocked"},
    "commit":   {"type": "string", "description": "move into done: the sha of the commit that carries the change, which done costs"}
  }
}`

// kanban is this extension's state: the board files, the HTTP server answering for them once a zot
// session or a serve command has started it, and the session this process is in.
type kanban struct {
	ext     *ext.Extension
	board   *Board
	server  *Server
	session string
}

// register declares the command, the tool and the one event this extension acts on. The SDK builds
// the wire frames from these calls, so this function never touches the protocol itself.
func (k *kanban) register() {
	e := k.ext

	// The board is built during the handshake: the SDK calls OnHello after hello_ack and before it
	// announces anything, and a one-shot run lands turn_end, session_end and exit in the same
	// millisecond, so any later event would be a coin toss. A board the human can open before the
	// first prompt is the point of the extension.
	e.OnHello(k.started)

	e.Command(cmdName, "Print this project's kanban board counts and the board URL: /kanban with no arguments.", func(args string) ext.Response {
		if args != "" {
			return ext.Errorf("usage: /kanban\n%s", help())
		}
		line, err := k.summary()
		if err != nil {
			return ext.Errorf("kanban: %v", err)
		}
		return ext.Display(line)
	})

	// The description is the rules' one published home, so a session reads the same text the Board answers with
	// instead of a paraphrase of it; toolFileRule is part of it, because the rule about reaching a Card with a
	// file tool belongs to the tool and not to help().
	e.Tool(toolName, toolDescription(), json.RawMessage(toolSchema), k.callTool)

	// session_start carries the session's alias and id, neither of which a tool-call handler ever sees,
	// and a Comment records which session wrote it. session_end is the one lifecycle event this
	// extension acts on for board counts:
	// turn_start and agent_end would deliver the same payload at the same instant for a one-shot run. A
	// hook is the wrong place for board work because it is scoped to one tool call, and no hook is a
	// completion claim.
	e.On("session_start", k.sessionBegan)
	e.On("session_end", k.sessionEnded)
}

// answer is what one tool call leaves with: the sentence the Agent reads back, and the line the human is told
// beside it - empty when there is nothing to tell, which is what a read and every "no change" answer is. A verb
// decides both and touches no host: callTool is the one place that knows zot has a log to notify, which is what
// makes a verb's sentence testable without launching this extension as a subprocess.
type answer struct {
	toAgent string
	toHuman string
}

// callTool is the one tool's dispatcher: one action discriminator, not one tool per verb. It is also
// the one place a call's two texts are handed over - the Agent's as the result, the human's as a log line - and
// the one place a refusal is written out.
func (k *kanban) callTool(args json.RawMessage) ext.ToolResult {
	var in toolArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return k.refused(fmt.Errorf("kanban arguments: %w", err))
	}
	var (
		said answer
		err  error
	)
	switch in.Action {
	case "", "add":
		said, err = k.addCard(in)
	case "comment":
		said, err = k.commentCard(in)
	case "edit":
		said, err = k.editCard(in)
	case "move":
		said, err = k.moveCard(in)
	case "read":
		said, err = k.readBoard(in)
	case "step":
		said, err = k.stepCard(in)
	default:
		err = fmt.Errorf("kanban: no action %q; this tool has add, comment, edit, move, read and step", in.Action)
	}
	if err != nil {
		return k.refused(err)
	}
	if said.toHuman != "" {
		k.ext.Notify("info", said.toHuman)
	}
	return ext.TextResult(said.toAgent)
}

// refused is where a tool refusal is written out: the words the Board or the door had for it, then help(), which
// names the version answering - a refusal is read as authority, and the incident was a manifest
// left at 0.31.0 while the tree was at 0.35.0, so a session read one version's rules while another was answering
// and had nothing on the page to tell it which. Every call used to append that text by hand, which is how a
// refusal could reach a session without it; now it is one function away from every one.
func (k *kanban) refused(err error) ext.ToolResult {
	return ext.TextErrorResult(err.Error() + "\n" + help())
}

// prove is the difference between a claim and a fact. Board.Add writes with os.Link and answers with the
// Card it wrote; this asks the board for that file by id and hands back its absolute path, and refuses
// when the board does not hold the Card being described. It exists because of the one failure this tool
// has already had: a stale install answered "added card 0736 to inbox" for a Card nothing on disk ever
// held. The path is also the answer a human can open, which a bare id is not.
func (k *kanban) prove(card Card) (string, error) {
	column, path, _, err := k.board.cardFile(card.ID)
	if err != nil {
		return "", fmt.Errorf("card %s was reported as filed and the board at %s does not hold it: %w",
			card.ID, k.board.Root, err)
	}
	if column != card.Column {
		return "", fmt.Errorf("card %s was filed into %s and the board has it in %s: %s",
			card.ID, card.Column, column, path)
	}
	return path, nil
}

// addCard files one card into inbox, the column the human triages out of, and answers with the file the
// add wrote rather than only the id the filing chose. Filing through AddAsAgent is what marks the Card new on
// the board until the human opens it.
func (k *kanban) addCard(in toolArgs) (answer, error) {
	card, err := k.board.AddAsAgent(FirstColumn, named(in.Title), named(in.Body), named(in.Priority), time.Now())
	if err != nil {
		return answer{}, err
	}
	path, err := k.prove(card)
	if err != nil {
		return answer{}, fmt.Errorf("kanban add: %w", err)
	}
	// The line the human is told keeps them current without claiming more than a card in the inbox; the Agent
	// gets the file, because the answer has to be checkable.
	return answer{
		toAgent: fmt.Sprintf("added card %s to %s: %s", card.ID, card.Column, path),
		toHuman: fmt.Sprintf("kanban: card in inbox: %s", card.Title),
	}, nil
}

// editCard changes what one Card says, in the Column it sits in: no Column and no Order reach it, because where
// a Card sits and how it is sorted are a drag's to name. Which hand captured the Card decides whether
// this door answers it at all, and the Board says so in its own words (EditAsAgent).
func (k *kanban) editCard(in toolArgs) (answer, error) {
	cardID := strings.TrimSpace(in.CardID)
	if cardID == "" {
		return answer{}, errors.New(`kanban edit: needs "card", the id of the Card to edit`)
	}
	edit, fields := requestedEdit(in)
	if len(fields) == 0 {
		return answer{}, errors.New(`kanban edit: names nothing to change - send one of "title", "body", "priority"`)
	}
	edited, err := k.board.EditAsAgent(cardID, edit, time.Now())
	if err != nil {
		return answer{}, err
	}
	said, notify := editSaid(edited, fields)
	return answer{toAgent: said, toHuman: notify}, nil
}

// requestedEdit is the CardEdit this call names, and the names of the fields it named, in the order its sentence
// reads them. A field the call left out is nil, which is what lets an edit of a title alone leave the body and
// the priority where they are.
func requestedEdit(in toolArgs) (CardEdit, []string) {
	var (
		edit   CardEdit
		fields []string
	)
	if in.Title != nil {
		edit.Title, fields = in.Title, append(fields, "title")
	}
	if in.Body != nil {
		edit.Body, fields = in.Body, append(fields, "body")
	}
	if in.Priority != nil {
		edit.Priority, fields = in.Priority, append(fields, "priority")
	}
	return edit, fields
}

// editSaid is the sentence an Agent gets for an Edit the Board answered, and the notice the human sees beside it:
// which fields the Card was changed in, and nothing at all when the Card already said them. The notice
// names the Card by its new title and quotes no field's content, the way every other notice does.
func editSaid(edited Write, fields []string) (said, notify string) {
	if !edited.Changed {
		return fmt.Sprintf("no change: card %s already says that", edited.ID), ""
	}
	changed := strings.Join(fields, ", ")
	return fmt.Sprintf("card %s edited in %s: %s", edited.ID, edited.Column, changed),
		fmt.Sprintf("kanban: card edited: %s (%s)", edited.Title, changed)
}

// commentCard files one Comment in a single call. A repeat of the same words is the Comment it
// already is: nothing is written and the answer names the ordinal that holds them, so a retry loop cannot
// duplicate a note. An "id" corrects that ordinal, under the rules applyComment carries.
func (k *kanban) commentCard(in toolArgs) (answer, error) {
	cardID := strings.TrimSpace(in.CardID)
	if cardID == "" {
		return answer{}, errors.New("kanban comment: needs \"card\", the id of the card to comment on")
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return answer{}, errors.New("kanban comment: needs \"text\", the Comment to file")
	}
	if in.CommentID < 0 {
		return answer{}, fmt.Errorf("kanban comment: \"id\" is a Comment ordinal on card %s, or left out for the next one on it, and %d is no ordinal anywhere", cardID, in.CommentID)
	}

	filed, err := k.board.AddComment(cardID, Comment{ID: in.CommentID, Text: text, By: byAgent, Session: k.session, At: time.Now()})
	if err != nil {
		return answer{}, err
	}
	// The line is the sentence the agent gets back, so both doors say the same thing about one filing, and a
	// Comment the Card already says reaches neither.
	said, notify := commentSaid(filed, in.CommentID, text)
	return answer{toAgent: said, toHuman: notify}, nil
}

// commentSaid is the sentence an Agent gets for a Comment the Board answered, and the notice the human sees
// beside it: the ordinal, the Card it is on, and whether the Agent said something new or reworded its own.
// A Comment the Card already says is answered without a line, and it is the Write that says so. The
// words themselves are read on the board, where an Agent's Comment paints the Card's comment count blue until
// somebody opens it.
func commentSaid(filed Write, ask int, text string) (said, notify string) {
	if !filed.Changed {
		return fmt.Sprintf("no change: Comment %d on card %s already says that", filed.commentID(text), filed.ID), ""
	}
	said = fmt.Sprintf("Comment %d filed on card %s", filed.commentID(text), filed.ID)
	if ask != 0 {
		said = fmt.Sprintf("Comment %d on card %s corrected", ask, filed.ID)
	}
	return said, "kanban: " + said
}

// stepCard works one of a Card's steps: a name files one, an ordinal marks it, and an ordinal with op opens it
// again or erases it. The Board answers this door only for the session holding the Card in doing, and says so in
// its own words (StepAsAgent). A step is not a Comment: the words that name the step are not reworded, so the
// door has no correction form.
func (k *kanban) stepCard(in toolArgs) (answer, error) {
	cardID := strings.TrimSpace(in.CardID)
	if cardID == "" {
		return answer{}, errors.New("kanban step: needs \"card\", the id of the Card whose step this is")
	}
	touched, err := k.board.StepAsAgent(StepRequest{
		Card:    cardID,
		Session: k.session,
		Text:    strings.TrimSpace(in.Text),
		Ordinal: in.StepOrdinal,
		Op:      in.Op,
	}, time.Now())
	if err != nil {
		return answer{}, err
	}
	said, notify := stepSaid(touched)
	return answer{toAgent: said, toHuman: notify}, nil
}

// stepSaid is the sentence an Agent gets for one step, and the notice the human sees beside it: the ordinal, the
// Card it is on, and what happened to it - filed, marked, opened again, erased. A call that changed nothing is
// answered without a line for the human, the way a Comment the Card already says is.
func stepSaid(touched StepWrite) (said, notify string) {
	switch touched.Did {
	case stepAlreadyFile:
		return fmt.Sprintf("no change: card %s already files that step: %s", touched.ID, touched.Step.Name), ""
	case stepAlreadyDone:
		return fmt.Sprintf("no change: step %d on card %s is already marked done: %s", touched.Step.ID, touched.ID, touched.Step.Name), ""
	case stepAlreadyOpen:
		return fmt.Sprintf("no change: step %d on card %s is already open: %s", touched.Step.ID, touched.ID, touched.Step.Name), ""
	}
	switch touched.Did {
	case stepUnmarked:
		said = fmt.Sprintf("step %d on card %s opened again: %s", touched.Step.ID, touched.ID, touched.Step.Name)
	case stepErased:
		said = fmt.Sprintf("step %d erased from card %s: %s", touched.Step.ID, touched.ID, touched.Step.Name)
	case stepMarked:
		said = fmt.Sprintf("step %d on card %s marked done: %s", touched.Step.ID, touched.ID, touched.Step.Name)
	default:
		said = fmt.Sprintf("step %d filed on card %s: %s", touched.Step.ID, touched.ID, touched.Step.Name)
	}
	return said, "kanban: " + said
}

// moveCard carries a Card from one Column to another, which is the file rename kept as the only way
// a Card changes Column. The rules it inherits from the model layer are set there:
// done is reachable only with the sha of the commit that carries the change, so a move into done that names
// no "commit" is refused before anything is renamed, and a name the repository behind the board has no
// commit of is refused too - a done Card is reopened by reverting the commit it names, so one naming a
// commit nothing has could never be. A Card sitting in trash is in no Column, so it is
// not movable; zot-kanban restore is the door that puts it back where a move can find it. The tool
// keeps no copy of those rules: the Board's Column gate answers them and this call reports its words.
func (k *kanban) moveCard(in toolArgs) (answer, error) {
	cardID := strings.TrimSpace(in.CardID)
	if cardID == "" {
		return answer{}, errors.New("kanban move: needs \"card\", the id of the card to move")
	}
	column := strings.TrimSpace(in.Column)
	if column == "" {
		return answer{}, errors.New("kanban move: needs \"column\", the column to move the card into (" +
			strings.Join(Columns, ", ") + ")")
	}
	moved, err := k.board.Move(cardID, column, strings.TrimSpace(in.Commit), time.Now(), AgentMove(k.session))
	if err != nil {
		return answer{}, err
	}
	// The sentence and the notice come out of what the Board wrote, not out of what this call asked for: the
	// Column the Card was read out of and the one it is in say whether it travelled, and Changed says whether a
	// Claim was written or the Card already said this.
	said, notify := moveSaid(moved, k.session)
	return answer{toAgent: said, toHuman: notify}, nil
}

// moveSaid is the sentence an Agent gets back for a Move the Board answered, and the notice the human sees
// beside it - an empty notice for a Move that changed nothing, because that is not worth a line on the board.
// Reading only the request would call a Card that was already there "moved", and would report a Claim the
// Board never wrote; both readings are in the Write.
func moveSaid(moved Write, session string) (said, notify string) {
	if moved.From == moved.Column && moved.Column != DoingColumn {
		return fmt.Sprintf("no change: card %s is already in %s", moved.ID, moved.Column), ""
	}
	if moved.From == moved.Column {
		// The only in-place Write Move makes in doing is the Claim it adds to a Card nobody was holding.
		if moved.Changed {
			return fmt.Sprintf("card %s was already in doing with nobody holding it, and is now claimed for this session", moved.ID),
				fmt.Sprintf("kanban: card taken into doing: %s (%s)", moved.Title, session)
		}
		if held := moved.Card.Claim; held != nil && held.Session == session {
			return fmt.Sprintf("no change: card %s is already in doing, held by this session", moved.ID), ""
		}
		return fmt.Sprintf("no change: card %s is already in %s", moved.ID, moved.Column), ""
	}
	switch moved.Column {
	case DoneColumn:
		return fmt.Sprintf("moved card %s from %s to done with commit %s", moved.ID, moved.From, moved.Commit),
			fmt.Sprintf("kanban: card filed done: %s (%s)", moved.ID, moved.Commit)
	case DoingColumn:
		return fmt.Sprintf("moved card %s from %s to doing, claimed for this session", moved.ID, moved.From),
			fmt.Sprintf("kanban: card taken into doing: %s (%s)", moved.Title, session)
	}
	return fmt.Sprintf("moved card %s from %s to %s", moved.ID, moved.From, moved.Column),
		fmt.Sprintf("kanban: card moved: %s, %s -> %s", moved.ID, moved.From, moved.Column)
}

// readBoard is what an Agent looks at before it picks anything up: the Columns an Agent works, and in each
// one the id to name the Card with, its priority, its title, how many Comments it carries and the session
// holding it, if any. backlog and done are absent unless named, because a read is about work to take and
// not about the human's list or the finished one. A read that names a Card answers that one Card
// instead, from whichever Column holds it: an id copied off the board is a question about one Card, and the
// Column is exactly the fact the copier cannot know. Which Columns a read walks, whether a name is a Column
// at all, and where a named Card's file is are the Board's answer (Read); this is the reading out of it.
func (k *kanban) readBoard(in toolArgs) (answer, error) {
	read, err := k.board.Read(ReadRequest{Column: in.Column, Card: in.CardID})
	if err != nil {
		return answer{}, fmt.Errorf("kanban read: %w", err)
	}
	// The answer says which question it answered: a read of one Card names the file it read that Card out of,
	// and a listing names none. A read tells the human nothing, because it is the Agent looking at what the
	// page already shows.
	if read.Path == "" {
		return answer{toAgent: strings.Join(listingLines(k.board.Root, read), "\n")}, nil
	}
	return answer{toAgent: strings.Join(cardLines(read.Cards[0], read.Path), "\n")}, nil
}

// listingLines are the lines a read's listing is made of. The board it read comes first, because a read that
// does not say which board it read is how a session and a human end up talking about two of them; then each
// Column the read walked, with how many Cards it holds; then those Cards in that Column's own order.
func listingLines(boardRoot string, read Read) []string {
	listed := map[string][]Card{}
	for _, c := range read.Cards {
		listed[c.Column] = append(listed[c.Column], c)
	}
	lines := make([]string, 0, len(read.Cards)+len(read.Columns)+1)
	lines = append(lines, "board "+boardRoot)
	for _, col := range read.Columns {
		lines = append(lines, fmt.Sprintf("%s %d", col, len(listed[col])))
		for _, c := range listed[col] {
			line := fmt.Sprintf("  %s  [%s]  %s", c.ID, c.Priority, c.Title)
			switch n := len(c.Comments); n {
			case 0:
			case 1:
				line += "  (1 comment)"
			default:
				line += fmt.Sprintf("  (%d comments)", n)
			}
			// A Card's steps are said on a listing too, because the count is what tells a session picking a Card
			// from the shelf that some of its work is already done.
			if steps := stepCount(c); steps != "" {
				line += "  " + steps + " steps"
			}
			if c.Claim != nil && c.Claim.Session != "" {
				line += "  held by " + c.Claim.Session
			}
			// A Card in done that names no commit, or names one the repository has no commit of, is said with
			// that on it rather than listed as finished work.
			if c.Contradiction != "" {
				line += "  contradicts: " + c.Contradiction
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// cardLines are the lines one Card read whole is told in - what a copied id asks to be told: this Card,
// wherever it sits. The Column comes first because it is the field that moves under you and the
// one an id copied off the board cannot carry. The file is named because a Card is a file and
// because the way to read a Card is to open it. Everything else the Card holds is here too, which a listing
// leaves out on purpose: a listing is the shelf to pick from, this is one Card read whole - the modal's
// question, asked of the tool.
func cardLines(card Card, path string) []string {
	lines := []string{
		fmt.Sprintf("card %s  in %s  [%s]  %s", card.ID, card.Column, card.Priority, card.Title),
		"file " + path,
		// Which hand captured the Card is the fact the Card's own words turn on: an edit from the tool answers
		// only one an agent filed, and a read that could not name the hand would leave that refusal a surprise.
		strings.Join(append(originFact(card), doneFacts(card)...), "  "),
	}
	if body := strings.TrimSpace(card.Body); body != "" {
		lines = append(lines, "body", indented(body))
	}
	// A Card with no steps says nothing about steps: the door is empty on such a Card rather than a heading with
	// nothing under it. A step is told by its ordinal, whether it is done, and its name - the three a session needs
	// to mark one without guessing which one it means, and the reason the ordinal is printed at all.
	if n := len(card.Steps); n != 0 {
		lines = append(lines, "steps "+stepCount(card))
		for _, s := range orderedSteps(card) {
			state := "·"
			if s.Done {
				state = "✓"
			}
			lines = append(lines, indented(fmt.Sprintf("%d %s %s", s.ID, state, s.Name)))
		}
	}
	if n := len(card.Comments); n == 0 {
		lines = append(lines, "no comments")
	} else {
		lines = append(lines, fmt.Sprintf("comments %d", n))
		for _, c := range card.Comments {
			by := c.By
			if by == "" {
				by = byHuman // a Comment typed into the file by hand has no author field
			}
			lines = append(lines, indented(fmt.Sprintf("%d  %s  %s  %s", c.ID, by, stamp(c.At), c.Text)))
		}
	}
	return lines
}

// originFact is the one fact about who captured the Card: "by agent" or "by human", and nothing at all when no
// door recorded one - a Card filed before origin existed carries no mark and no by-line, the same absence the
// modal shows.
func originFact(c Card) []string {
	facts := []string{
		fmt.Sprintf("order %d", c.Order),
		"created " + stamp(c.Created),
		"updated " + stamp(c.Updated),
	}
	if c.Origin != "" {
		facts = append(facts, "by "+c.Origin)
	}
	return facts
}

// doneFacts are the facts a Card carries only when something has been said about it: the commit that filed it
// done, what is wrong with that commit when the board can see something wrong, and the session holding it.
// They are the same three a listing puts after a Card, so one Card and a shelf of them
// say the same words about the same Card.
func doneFacts(c Card) []string {
	out := make([]string, 0, 3)
	if c.Commit != "" {
		out = append(out, "commit "+c.Commit)
	}
	if c.Contradiction != "" {
		out = append(out, "contradicts: "+c.Contradiction)
	}
	if c.Claim != nil && c.Claim.Session != "" {
		out = append(out, "held by "+c.Claim.Session+" since "+stamp(c.Claim.At))
	}
	return out
}

// stamp is a timestamp an Agent can read back: UTC to the second, and a word in place of the zero time a
// hand-written Card carries. The board prints the same facts to the minute, for a human reading a screen.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "no date"
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// indented puts a Card's own text under the line that names it, so a body or a Comment running over more than
// one line cannot be read as more facts about the Card.
func indented(text string) string {
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}

// sessionBegan records which zot session this process is in, because a Comment says who wrote it and
// a rewrite is checked against that name. zot names a persisted conversation with a readable alias, and
// session_start carries it; what the board stamps is the alias, and the hex SessionID only for a session
// with no alias - a legacy transcript, an explicit-path session, or a host older than 0.4.66 that sends
// none. A Claim, a Comment and the refusal that names a holder then all read the same word.
func (k *kanban) sessionBegan(ev ext.Event) {
	if ev.SessionAlias != "" {
		k.session = ev.SessionAlias
		return
	}
	if ev.SessionID != "" {
		k.session = ev.SessionID
	}
}

// noOpenEnv is the human's veto on being opened: set to anything but 0, and a board start prints its
// address instead of handing it to the browser. A zot session gets no flags, so the environment is the
// only door a human has to this.
const noOpenEnv = "ZOT_KANBAN_NO_OPEN"

// openArgs names the command this OS uses to hand a URL to a browser, and false for an OS nothing here
// opens for. start is a cmd builtin rather than a program, so Windows goes through cmd /c, where the
// empty argument is the window title a bare start wants before the URL.
func openArgs(goos, boardURL string) (string, []string, bool) {
	switch goos {
	case "linux":
		return "xdg-open", []string{boardURL}, true
	case "darwin":
		return "open", []string{boardURL}, true
	case "windows":
		return "cmd", []string{"/c", "start", "", boardURL}, true
	}
	return "", nil, false
}

// startCommand is the opener actually run, held in a variable so a test can watch the call rather than
// open a browser. Start rather than Run: some openers stay attached to the URL they were given, and a
// board that serves is not going to wait on one.
var startCommand = func(name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return err
	}
	return exec.Command(name, args...).Start()
}

// openBoard hands the bound address to the human's browser, and returns the word to add to the line that
// names the board when it could not: nothing when this OS has no opener or the human vetoed one, and the
// opener's name when the machine does not have it, because a URL the board could not open is one the
// human now has to open by hand.
func openBoard(goos, boardURL string) string {
	if v := os.Getenv(noOpenEnv); v != "" && v != "0" {
		return ""
	}
	name, args, ok := openArgs(goos, boardURL)
	if !ok {
		return ""
	}
	if err := startCommand(name, args...); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return " (" + name + " not found: open " + boardURL + " in a browser by hand)"
		}
		return " (" + name + ": " + err.Error() + ")"
	}
	return ""
}

// started binds the board to the project zot reported, starts its server, and reports the URL.
//
// A failure has to be visible: an extension that registers nothing is otherwise silent, and stderr
// goes to a log file nobody is looking at (lessons §5). A startup notification is the only frame
// that can report a startup problem at this point, because the command and tool may not exist yet.
func (k *kanban) started(host ext.HostInfo) {
	k.board = NewBoard(host.CWD)
	if err := k.board.ensure(); err != nil {
		k.ext.NotifyStartup("error", "kanban: no board in "+host.CWD+": "+err.Error())
		return // command and tool still register, and their errors say why there is no board
	}
	// The host is the one that knows where it loaded this extension from, and the header says so: the
	// project's files and the code serving them need not sit in the same place.
	srv, err := NewServer(host.CWD, host.ExtensionDir)
	if err != nil {
		k.ext.NotifyStartup("error", "kanban: "+err.Error())
		return
	}
	k.board = srv.board
	if err := srv.Listen(defaultPort); err != nil {
		k.ext.NotifyStartup("error", "kanban: board files exist but nothing is serving them: "+err.Error())
		return
	}
	k.server = srv
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(os.Stderr, "kanban: board server: %v\n", err)
		}
	}()
	k.ext.NotifyStartup("success", startLine(srv)+openBoard(runtime.GOOS, srv.boardURL()))
}

// startLine is what a board that has begun announces: the version announcing, the address serving, and the tree
// whose files it reads. The startup notification and `zot-kanban serve` both say it, and the version is in it
// because a log line a session never saw is one it cannot read the generation off.
func startLine(srv *Server) string {
	return "kanban " + version + " board " + srv.boardURL() + " (" + srv.board.Root + ")"
}

// sessionEnded reports the board to the human when the session is over. Event handlers are
// observational - the host ignores their reply - so this is a notification rather than agent
// context, and it reports counts rather than asserting that anything is done.
func (k *kanban) sessionEnded(ev ext.Event) {
	if k.board == nil {
		return
	}
	line, err := k.summary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kanban: session_end: %v\n", err)
		return
	}
	k.ext.Notify("info", "kanban at session end: "+line)
}

// summary is the board report both the command and the session_end tool log line show: one read, every column,
// and the URL that is actually answering right now rather than the default one.
func (k *kanban) summary() (string, error) {
	cols, counts, err := k.board.CountByColumn()
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(cols))
	for i, col := range cols {
		// The six Columns are the board's shape and are always named; closed is kept work, and a board with
		// none of it has nothing to report about it.
		if col == ClosedColumn && counts[i] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d", col, counts[i]))
	}
	if k.server == nil {
		return strings.Join(parts, " | ") + "\nboard not served: zot-kanban serve (kanban " + version + ")", nil
	}
	return strings.Join(parts, " | ") + "\nboard " + k.server.boardURL() + " (kanban " + version + ")", nil
}

func main() {
	if len(os.Args) > 1 {
		os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr))
	}
	k := &kanban{ext: ext.New(extName, version)}
	k.register()
	if err := k.ext.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "zot-kanban:", err)
		os.Exit(1)
	}
}

const usageText = `zot-kanban - project-scoped kanban board for zot

  zot-kanban                        run as a zot extension (stdin/stdout protocol; what zot execs)
  zot-kanban serve [--port N]       serve this project's board without a zot session; a port you name is bound exactly, a default one walks up when taken
  zot-kanban url                    print the URL of the board being served right now
  zot-kanban close <id>             close a done Card: its file moves to .kanban/closed/, kept and not shown
  zot-kanban close --all-done       close every done Card whose commit evidence and whose steps hold up, leaving one that fails either in done
  zot-kanban reopen <id>            bring a closed Card back into done, filed at the end of that Column's Order
  zot-kanban delete <id>            take a Card off the board: its file moves to .kanban/trash/ (never a done or closed Card)
  zot-kanban restore <id> [col]     put a deleted Card back into a Column (inbox when none is named)
  zot-kanban version                print the extension version
`

// whyLeftInDone is the bulk door's reason for leaving a done Card where it is: the finding that its commit
// evidence does not hold up, or a step the Card wrote and never marked - the same two findings the singular close
// refuses over, said here in the words the shell prints.
func whyLeftInDone(c Card) string {
	if c.Contradiction != "" {
		return c.Contradiction
	}
	return openStepWords(unmetSteps(c))
}

// cli is the standalone face of the extension, kept apart from extension mode so it can be tested
// without a host. serve exists because a board is useful outside a zot session - a triage pass in
// the morning - and url exists because the port may have fallen back, and guessing is the failure
// mode the plan set out to avoid. delete and restore are there because a Card is a file and pruning a
// board from a shell is the same door the modal's delete button uses, and close and reopen are the
// same shape for finished work; none of them is offered to the agent tool, which files Cards and
// Comments and takes nothing off the board.
func cli(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("zot-kanban", flag.ContinueOnError)
	fs.SetOutput(stdout)
	fs.Usage = func() { fmt.Fprint(stdout, usageText) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	sub := fs.Arg(0)
	switch sub {
	case "", "help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "version":
		if code, ok := verbLeftovers(sub, fs.Args()[1:], stdout, stderr); !ok {
			return code
		}
		fmt.Fprintf(stdout, "zot-kanban %s\n", version)
		return 0
	case "serve":
		// The verb's own FlagSet: the one above stops parsing at the first non-flag argument, which is
		// the verb, so anything after it reaches here as a positional and --port used to be an argument
		// this CLI never read. A port named here is bound exactly; the walk upward stays what a board
		// nobody named a port for gets.
		sfs := flag.NewFlagSet("zot-kanban serve", flag.ContinueOnError)
		sfs.SetOutput(stdout)
		sfs.Usage = func() { fmt.Fprint(stdout, "usage: zot-kanban serve [--port N]\n\n"+usageText) }
		port := sfs.Int("port", defaultPort, "port to bind; a taken one is refused by name, not walked past")
		if err := sfs.Parse(fs.Args()[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if extra := sfs.Arg(0); extra != "" {
			fmt.Fprintf(stderr, "zot-kanban serve: unexpected argument %q\n", extra)
			sfs.Usage()
			return 2
		}
		named := false
		sfs.Visit(func(f *flag.Flag) { named = named || f.Name == "port" })
		// Run from the command line there is no host to ask, so the binary is the clue: an installed one sits
		// under an install directory, a repository build sits in the tree it was built in.
		exe, err := os.Executable()
		if err != nil {
			exe = mustCWD()
		}
		srv, err := NewServer(mustCWD(), exe)
		if err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		if err := srv.board.ensure(); err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		bind := srv.Listen
		if named {
			bind = srv.ListenExactly
		}
		if err := bind(*port); err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		fmt.Fprintln(stdout, startLine(srv)+openBoard(runtime.GOOS, srv.boardURL()))
		if err := srv.Serve(); err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		return 0
	case "url":
		if code, ok := verbLeftovers(sub, fs.Args()[1:], stdout, stderr); !ok {
			return code
		}
		u, err := liveURL(NewBoard(mustCWD()))
		if err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		fmt.Fprintln(stdout, u)
		return 0
	case "close":
		// Closing is the human's door onto the record: a done Card's file moves to .kanban/closed/, where the
		// board still reads it and paints it only when a view asks. The verb has two shapes - one id
		// a human is looking at, and --all-done over a column of finished work, which is the door only the shell
		// has and which checks each Card against its own commit before moving it.
		cfs := flag.NewFlagSet("zot-kanban close", flag.ContinueOnError)
		cfs.SetOutput(stdout)
		cfs.Usage = func() {
			fmt.Fprint(stdout, "usage: zot-kanban close <id>\n       zot-kanban close --all-done\n\n"+usageText)
		}
		allDone := cfs.Bool("all-done", false, "close every done Card whose commit evidence and whose steps hold up")
		if err := cfs.Parse(fs.Args()[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		board := NewBoard(mustCWD())
		if *allDone {
			if extra := cfs.Arg(0); extra != "" {
				fmt.Fprintf(stderr, "zot-kanban close --all-done names no Card id: %q\n", extra)
				cfs.Usage()
				return 2
			}
			closed, skipped, err := board.CloseAllDone()
			if err != nil {
				fmt.Fprintln(stderr, "zot-kanban:", err)
				return 1
			}
			fmt.Fprintf(stdout, "closed %d done Card(s) into %s, where the board reads them on demand\n",
				len(closed), board.closedDir())
			for _, c := range skipped {
				fmt.Fprintf(stdout, "left %s in done: %s\n", c.ID, whyLeftInDone(c))
			}
			return 0
		}
		id := cfs.Arg(0)
		if id == "" {
			fmt.Fprintln(stderr, "zot-kanban: close wants a Card id: zot-kanban close <id> - only a Card in done can be closed")
			return 2
		}
		if err := board.Close(id); err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		fmt.Fprintf(stdout, "closed card %s: its file is now %s, where the board keeps it and reads it on demand\n",
			id, filepath.Join(board.closedDir(), id+cardExt))
		return 0
	case "reopen":
		// The way back out of closed is the same human door: the file moves into done and is filed at the end of
		// that Column's Order, so the work that has to be worked again is what a reader of done sees first.
		// Only a Card sitting in closed is reopened - restore is the undo of a delete and
		// answers a Card in Trash - and no Agent door offers either.
		if code, ok := verbLeftovers(sub, fs.Args()[1:], stdout, stderr); !ok {
			return code
		}
		id := fs.Arg(1)
		if id == "" {
			fmt.Fprintln(stderr, "zot-kanban: reopen wants a Card id: zot-kanban reopen <id> - only a Card in closed can be reopened")
			return 2
		}
		board := NewBoard(mustCWD())
		reopened, err := board.Reopen(id, time.Now())
		if err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		fmt.Fprintf(stdout, "reopened card %s: its file is back in %s at order %d, the end a reader of that Column sees first\n",
			reopened.ID, DoneColumn, reopened.Order)
		return 0
	case "delete":
		// The id is the filename stem, so it arrives from the modal's copy-id button rather than being
		// retyped from a title. Nothing is destroyed here: the file moves to .kanban/trash/.
		id := fs.Arg(1)
		if id == "" {
			fmt.Fprintln(stderr, "zot-kanban: delete wants a Card id: zot-kanban delete <id> - the modal's copy-id button is how to get one")
			return 2
		}
		board := NewBoard(mustCWD())
		if err := board.Delete(id); err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted card %s: its file is now %s, and zot-kanban restore %s puts it back\n",
			id, filepath.Join(board.trashDir(), id+cardExt), id)
		return 0
	case "restore":
		id := fs.Arg(1)
		if id == "" {
			fmt.Fprintln(stderr, "zot-kanban: restore wants a Card id: zot-kanban restore <id> [column]")
			return 2
		}
		column := fs.Arg(2)
		if column == "" {
			column = FirstColumn
		}
		if !validColumn(column) {
			fmt.Fprintf(stderr, "zot-kanban: unknown column %q (want one of %s)\n", column, strings.Join(Columns, ", "))
			return 2
		}
		if err := NewBoard(mustCWD()).Restore(id, column); err != nil {
			fmt.Fprintln(stderr, "zot-kanban:", err)
			return 1
		}
		fmt.Fprintf(stdout, "restored card %s into %s\n", id, column)
		return 0
	default:
		fmt.Fprintf(stderr, "zot-kanban: unknown command %q\n\n%s", sub, usageText)
		return 2
	}
}

// verbLeftovers answers what a verb that takes no arguments does with the words the FlagSet above left
// unparsed, since that FlagSet stops at the verb itself: a help request becomes the usage, and a flag
// the verb has never offered is refused by name instead of being walked past into silence. Verbs that
// take an id, delete and restore, are not asked, because an id is the positional they work on.
func verbLeftovers(verb string, rest []string, stdout, stderr io.Writer) (int, bool) {
	for _, arg := range rest {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			fmt.Fprint(stdout, usageText)
			return 0, false
		}
		if strings.HasPrefix(arg, "-") {
			fmt.Fprintf(stderr, "zot-kanban %s: no such flag %q\n", verb, arg)
			return 2, false
		}
	}
	return 0, true
}

func mustCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zot-kanban: cannot resolve the project directory:", err)
		os.Exit(1)
	}
	return cwd
}

// liveURL returns the address the last session recorded for this board, but only if something
// answers there. Printing a stale hint as a fact is the exact failure the plan named; the probe
// turns it into a claim that is checked before it is believed.
func liveURL(board *Board) (string, error) {
	raw, err := os.ReadFile(filepath.Join(board.Root, urlFile))
	if err != nil {
		return "", errors.New("no board is being served here: run zot-kanban serve, or open a zot session")
	}
	stored := strings.TrimSpace(string(raw))
	u, err := url.Parse(stored)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("the recorded board address %q is not a URL", stored)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 300*time.Millisecond)
	if err != nil {
		return "", fmt.Errorf("no board answering at %s, though a past session recorded it: zot-kanban serve", stored)
	}
	_ = conn.Close()
	return stored, nil
}
