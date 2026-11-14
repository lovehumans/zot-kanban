package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// card.go is the Card and what hangs off it: the Comment, the Claim and the Step a Card's file carries, the slug
// an id is cut from, the ordinals a Card issues and never reissues, the decoding and encoding of the file's
// fields, and the readings a face takes off a Card - its ladder, its counts, whose Origin it filed. A reading is
// not a field of the file, and nothing here opens one.

// A Comment is a note on a Card, filed inside the Card's own file. Only text is required:
// a human typing into the file writes text and nothing else, and the board shows that.
type Comment struct {
	ID      int       `json:"id,omitempty"`
	At      time.Time `json:"at,omitempty"`
	By      string    `json:"by,omitempty"`
	Session string    `json:"session,omitempty"`
	Text    string    `json:"text"`
}

// A Claim records which zot session holds a Card, and when it took it. The name it carries is the
// session's readable alias, or its hex id when it has no alias. A move into doing that
// an Agent asked for writes it; nothing else does, because a human move carries no session. A Card held
// by a session is worked by that session: another Agent's move of it is refused, and the way a claim is
// released is a human drag out of doing.
type Claim struct {
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

// Step is one small piece of work on a Card, filed by the session working it: an ordinal this Card issued and
// never reissues, a name short enough to say one small task in one line, and whether it is done. A Step is not
// a Card and nothing about a Card leaks into it: no Column, no id the Board issues, no Order, no Claim, no
// Comments, and it dies with the Card's file.
type Step struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Done bool   `json:"done"`
}

type Card struct {
	Title    string    `json:"title"`
	Body     string    `json:"body,omitempty"`
	Priority string    `json:"priority"`
	Order    int       `json:"order"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Commit   string    `json:"commit,omitempty"`
	Claim    *Claim    `json:"claim,omitempty"`

	// Origin is who captured this Card: byAgent for one an Agent filed with the kanban tool, byHuman for one a
	// person typed into the board's composer. It is recorded once at creation and never edited. A Card captured
	// before the field existed carries none, and the board gives it no new mark.
	Origin string `json:"origin,omitempty"`

	Comments []Comment `json:"comments,omitempty"`

	// Steps are this Card's small pieces of work, in the order they were filed. They exist for the session
	// working the Card: a step marked done is the board holding what the agent did, so a Card read after a
	// context compaction still says where the work got to.
	Steps []Step `json:"steps,omitempty"`

	// StepsIssued is the highest step ordinal this Card has ever issued, which is what keeps "never reused"
	// true across a delete: the highest-numbered step can be deleted, and without this record the next step
	// filed would take that number back, so a session's memory of what "step 3" meant would stop being
	// trustworthy.
	StepsIssued int `json:"steps_issued,omitempty"`

	Column string `json:"-"` // the directory it was found in
	ID     string `json:"-"` // the filename stem

	// Contradiction says why this Card's place in done does not hold up, and is empty when it holds up:
	// a Card filed done with no commit named, or with a name the repository behind the board has
	// no commit of. It is a reading of the Card, never a field of its file, and it is what the board shows
	// instead of pretending a done Card is fine.
	Contradiction string `json:"-"`

	// CommitOID is the full object name git answered for a done Card's commit, empty when the repository answered
	// nothing. A Card may be filed with an abbreviation - `git log --oneline` hands out seven digits - and this is
	// the name the repository actually knows the change by. Like Contradiction it is a reading of the Card and the
	// repository, never a field of its file, and it is what the Card view hands the clipboard.
	CommitOID string `json:"-"`

	// Unseen says no one has opened this Card since an Agent filed it. Like Contradiction it is a reading of
	// the Card and never a field of its file: the fact itself sits in .kanban/unseen, which is not
	// git-tracked.
	Unseen bool `json:"-"`

	// UnseenComments says this Card carries a Comment no one has opened since an Agent filed it. Like Unseen it
	// is a reading of the Card and not a field of its file: the ids sit in .kanban/unseen_comments.
	UnseenComments bool `json:"-"`

	// NamesHolder says this board has more than one Session holding Cards, which is when a Card's face prints
	// which one holds it. While one Session holds everything held, the name repeats what the thick edge on the
	// Card already says. It is a reading of the whole board, so a filter showing one claimed Card
	// does not hide the name a board with two holders has reason to print.
	NamesHolder bool `json:"-"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugLimit is how long the readable half of a Card id may be, and it is the length a Step's name is capped at
// (stepNameLimit): both are the words the board prints on a line, an id being the Card's title again as a slug,
// and neither is worth reading past that. A slug is ASCII throughout - slugRe is what leaves it - so
// counting it in bytes is counting it in characters.
const slugLimit = 42

// commentID reports the id the Comment with this text ended up with, which is what a confirmed write
// quotes back to the agent.
func (c Card) commentID(text string) int {
	for _, cm := range c.Comments {
		if cm.Text == text {
			return cm.ID
		}
	}
	return 0
}

// nextCommentID is the ordinal the next Comment on this Card will take. Comment ids are ordinals within one
// Card and are never reused, so a writer that has read the Card knows the number its own Comment lands on -
// which is what makes filing the same Comment twice detectable.
func nextCommentID(comments []Comment) int {
	last := 0
	for _, c := range comments {
		if c.ID > last {
			last = c.ID
		}
	}
	return last + 1
}

// repairStepOrdinals gives every step a number of its own: a missing ordinal, or one another step already holds,
// becomes the next free number above every ordinal this Card holds or has ever issued, and that highest number is
// answered back so both the reading and the step door count from the same record. It is the whole of what the board
// repairs about a step, and it is run on the same words by normalize when a Card is read and by applyStep when a
// step is written, which is what keeps "the ordinal you read is the step it names" true across a hand that wrote
// two steps with one number.
func repairStepOrdinals(steps []Step, issued int) ([]Step, int) {
	if issued < 0 {
		issued = 0
	}
	for _, s := range steps {
		if s.ID > issued {
			issued = s.ID
		}
	}
	taken := make(map[int]bool, len(steps))
	next := issued + 1
	for i := range steps {
		s := &steps[i]
		if s.ID <= 0 || taken[s.ID] {
			s.ID = next
			next++
		}
		if s.ID > issued {
			issued = s.ID
		}
		taken[s.ID] = true
	}
	return steps, issued
}

// nextStepID is the ordinal the next Step on this Card will take. Like a Comment's, a step ordinal is an ordinal
// within one Card and is never reused: it is read off the highest number this Card ever issued rather than off
// the steps it still holds, because a step can be deleted and the number it wore stays spent.
func nextStepID(steps []Step, issued int) int {
	last := issued
	for _, s := range steps {
		if s.ID > last {
			last = s.ID
		}
	}
	return last + 1
}

// slug turns a title into the human-readable half of a card id.
func slug(title string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "card"
	}
	if len(s) > slugLimit {
		// Trimmed on the cut end too, because a title whose limit-th character falls in a run of punctuation
		// would end the readable half on a "-": the mark that separates it from the date and from a -2 suffix,
		// and so a dash a hand would read as part of the name. The half cannot come out empty - s begins with a
		// letter or digit, since the Trim above took the leading run off.
		return strings.TrimRight(s[:slugLimit], "-")
	}
	return s
}

// normalize repairs what a hand-edited file may contain: a value the UI cannot otherwise
// display is clamped to the nearest legal one (lessons §5).
func (c *Card) normalize(now time.Time) {
	switch c.Priority {
	case priorityHi, priorityLo:
	default:
		c.Priority = priorityMi
	}
	if c.Order <= 0 {
		c.Order = orderStep
	}
	if c.Created.IsZero() {
		c.Created = now
	}
	if c.Updated.IsZero() {
		c.Updated = c.Created
	}
	// A Comment with no text is nothing to show, and a hand edit can leave one behind.
	kept := c.Comments[:0]
	for _, cm := range c.Comments {
		if strings.TrimSpace(cm.Text) != "" {
			kept = append(kept, cm)
		}
	}
	c.Comments = kept

	// A Step's ordinal is the one thing about it the board cannot read without: it is how a session names the
	// step it means, so a missing one, or a number two steps were handed, becomes the next free ordinal above
	// everything this Card has issued. Nothing else about a step is repaired here. A name's length, a newline it
	// carries and how many steps a Card holds are the step door's rules alone, so a Card whose steps a hand
	// wrote reads as the nine steps it says it is.
	if len(c.Steps) > 0 {
		c.Steps, c.StepsIssued = repairStepOrdinals(c.Steps, c.StepsIssued)
	}
}

// agentOrigin is the Card an Agent's door captured. The board says it without a word for it: the face
// wears its Priority stripe on the right edge rather than the left. A Card whose Origin was never
// recorded is nobody's to mark, so its stripe stays where a human's sits.
func agentOrigin(c Card) bool { return c.Origin == byAgent }

// commentCountTitle is the sentence a Card's face keeps when its Comment count leaves the word out and prints a
// number beside Tabler's bubble: the words a reader is told when they rest on the count, the same arrangement the
// step count has. An unread count names itself unread, because the colour that
// lights it says only that much; a read one states the total. Like stepCount it is a reading of the
// Card, never a field of its file.
func commentCountTitle(c Card) string {
	word := "Comments"
	if len(c.Comments) == 1 {
		word = "Comment"
	}
	if c.UnseenComments {
		return fmt.Sprintf("%d %s an agent filed, and nobody has opened the Card since", len(c.Comments), word)
	}
	return fmt.Sprintf("%d %s on this Card", len(c.Comments), word)
}

// stepCount is the "x/n" a Card's face prints for its steps: how many are done over how many the Card holds. A
// Card with no steps prints nothing at all, which keeps a Card nobody stepped exactly as it was. Like
// Contradiction it is a reading of the Card, never a field of its file.
func stepCount(c Card) string {
	if len(c.Steps) == 0 {
		return ""
	}
	done := 0
	for _, s := range c.Steps {
		if s.Done {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(c.Steps))
}

// orderedSteps are a Card's steps in ordinal order, which is the order they were filed in and the order a Card
// is read in. The Card's own slice stays where it is: a hand that wrote the steps out of order keeps them in its
// file as it wrote them and reads back sorted rather than rewritten.
func orderedSteps(c Card) []Step {
	if len(c.Steps) == 0 {
		return nil
	}
	return stepsOrdered(append([]Step{}, c.Steps...))
}

// stepsOrdered sorts a slice of steps by the ordinal the Card issued them with. It is the same reading the
// page's list and the Close door's refusal are built from, so neither carries its own sort.
func stepsOrdered(steps []Step) []Step {
	sort.Slice(steps, func(i, j int) bool { return steps[i].ID < steps[j].ID })
	return steps
}

// unmetSteps are the steps a finished Card still shows undone: an unmet step, which the board paints on the
// face and the modal lists in the red of what does not hold up. done costs the sha of the commit that carries the
// change, and a step is not that evidence in either direction, so a Move into done is never refused over an open
// step - only keeping a Card as finished work is, which is what close answers. A kept Card
// in closed carries the same reading, because its done-ness is the one its move into done rested on.
func unmetSteps(c Card) []Step {
	if c.Column != DoneColumn && c.Column != ClosedColumn {
		return nil
	}
	return openSteps(c.Steps)
}

// openSteps are the ones of a Card's steps it still shows undone, in ordinal order. It is the reading the face's
// unmet paint, the Close door's refusal and the shell's reason for leaving a Card in done are all built from, so
// none of them carries its own filter or sort.
func openSteps(steps []Step) []Step {
	var open []Step
	for _, s := range stepsOrdered(append([]Step{}, steps...)) {
		if !s.Done {
			open = append(open, s)
		}
	}
	return open
}

// openStepsOf is the Close door's reading of a Card file's steps: which of them the Card still shows undone, in
// ordinal order, read off the bytes the door has already read rather than by turning the file into a Card the way
// a render does. A Card whose steps a hand wrote out of shape is named as what could not be read.
func openStepsOf(raw []byte) ([]Step, error) {
	fields, err := decodeCard(raw)
	if err != nil {
		return nil, err
	}
	steps, err := stepList(fields["steps"])
	if err != nil {
		return nil, err
	}
	return openSteps(steps), nil
}

// openStepWords names the steps a refused close found open, ordinal first and name after it: the ordinal is how a
// session marks a step, and the name is what a human reads in the modal to decide which box to tick before
// closing the Card again.
func openStepWords(open []Step) string {
	if len(open) == 1 {
		return fmt.Sprintf("step %d %q is still open", open[0].ID, open[0].Name)
	}
	named := make([]string, 0, len(open))
	for _, s := range open {
		named = append(named, fmt.Sprintf("%d %q", s.ID, s.Name))
	}
	return fmt.Sprintf("steps %s are still open", strings.Join(named, ", "))
}

// stepRung is one rung of the ladder a Card's steps paint in its own view: either the step this Card holds at
// that ordinal, or the ordinal itself when the step that wore it was erased. A rung is not a step and carries no
// state beyond which step it holds - it is how the numbers a Card spent stay visible on the Card that spent
// them, so a hand reading the ladder reads a spent number as one rather than as a step whose name went missing.
type stepRung struct {
	Step   Step
	Erased bool
}

// cardLadder is a Card's ladder read for its own view: every ordinal the Card issued, up to the highest it ever
// filed, with the steps it holds drawn as rungs and the numbers it erased between them drawn as rungs of their
// own. The count a Card wears, the red of an unmet step and the cap on its live steps are readings of the steps
// held - stepCount, openSteps, maxLiveSteps - and only a Card's own view draws the wider one, because a spent
// number says something about the Card's numbering rather than about the work left to do. A Card whose ladder a
// hand wrote with a number no rung wore prints that number as erased, which is what its file says it issued.
func cardLadder(c Card) []stepRung {
	held := orderedSteps(c)
	last := c.StepsIssued
	if n := len(held); n > 0 && held[n-1].ID > last {
		last = held[n-1].ID
	}
	rungs := make([]stepRung, 0, last)
	at := 0
	for ordinal := 1; ordinal <= last; ordinal++ {
		if at < len(held) && held[at].ID == ordinal {
			rungs = append(rungs, stepRung{Step: held[at]})
			at++
			continue
		}
		rungs = append(rungs, stepRung{Step: Step{ID: ordinal}, Erased: true})
	}
	return rungs
}

// stepsFull is the modal's reading of the cap the step door counts: a Card already holding as many live steps as
// that door will file, where the filing control is drawn shut rather than asked. A Card whose steps a hand wrote
// into its own file is full by this reading too, which is why the control shuts rather than the door moving.
func stepsFull(c Card) bool { return len(c.Steps) >= maxLiveSteps }

// steppable is the modal's reading of handStepGate: which Card's steps the page may put a live box on. It is asked
// of gate.go rather than spelled out in a template, the way card.fragment.html asks movable and agentOrigin, so the
// boxes a human sees and the refusals the same click can get come from one reading.
func steppable(c Card) bool { return stepsLocked(c) == "" }

// stepsLocked is why the page draws this Card's boxes disabled, in the words the step door itself would refuse a
// click with: a Card a session is holding in doing, and a Card kept in closed. An empty answer is a Card whose steps
// a hand may mark - inbox, ready, blocked, a doing Card nobody has taken up, and every done Card, whatever its
// file's claim line says.
func stepsLocked(c Card) string {
	switch {
	case c.Column == ClosedColumn:
		return ruleStepClosed
	case c.Column == DoingColumn && c.Claim != nil:
		return ruleStepHeld
	}
	return ""
}

// readCard turns one Card file's bytes into a Card carrying the Column and id its directory gives it.
func readCard(raw []byte, col, id string, now time.Time) (Card, error) {
	var c Card
	if err := json.Unmarshal(raw, &c); err != nil {
		return Card{}, fmt.Errorf("card %s: the file no longer parses: %w", id, err)
	}
	c.Column, c.ID = col, id
	c.normalize(now)
	return c, nil
}

// applyHumanComment files one Comment at the ordinal its writer meant, or says why that ordinal is not this
// Comment's to take, and answers with the Comment that ends up on the Card - whether this call filed it or
// found it filed already. Whether the Card is written at all, and with what date, is rewrite's: a Card that
// already says this keeps the bytes it has.
func applyHumanComment(fields map[string]any, c Comment, want int) (Comment, error) {
	existing, err := commentList(fields["comments"])
	if err != nil {
		return Comment{}, err
	}
	next := nextCommentID(existing)
	// The Card already says this. Filing it again changes nothing, so nothing is written and the Comment that
	// is there is what comes back - the same reading the agent's door gives before it files (which carries as a comparison of parsed values),
	// and what makes a double click harmless. Comments are
	// filed once and never edited; a Card is not a log.
	for _, e := range existing {
		if e.Text == c.Text {
			return e, nil
		}
	}
	if want != 0 && want != next {
		for _, e := range existing {
			if e.ID == want {
				return Comment{}, fmt.Errorf("%w: #%d already says %q", ErrCommentTaken, e.ID, e.Text)
			}
		}
		return Comment{}, fmt.Errorf("%w: the next one on this Card is #%d", ErrCommentOrdinal, next)
	}
	c.ID = next
	fields["comments"] = append(existing, c)
	return c, nil
}

// cardBytes writes a decoded Card as the file this board uses for every Card: one JSON object, indented with
// two spaces, its top-level keys in name order, which is what encoding/json gives a map. save and rewrite
// write through it, so a Card is born in the shape a Comment, an edit or a move will keep, and a later write
// changes the lines it names instead of recasting the file.
//
// Handing the Comments over as []Comment is part of that, because a decoded file holds them as []any of maps
// and a map encodes its keys in name order: an edit or a move that touched no Comment would re-flow every
// Comment block on the Card, its id sorting after its moment and its author. Read through commentList, a
// Comment keeps the struct's own field order. A comments value commentList cannot read is left as
// the file wrote it: this is not the door that settles a hand's broken field, and a Move carries it anyway.
func cardBytes(fields map[string]any) ([]byte, error) {
	if comments, err := commentList(fields["comments"]); err == nil && comments != nil {
		fields["comments"] = comments
	}
	// The same reason as the Comments: a decoded file holds steps as []any of maps, and a map encodes its keys
	// in name order, so a write that touched no step would re-flow every step block on the Card. Read through
	// stepList, a step keeps the struct's own field order.
	if steps, err := stepList(fields["steps"]); err == nil && steps != nil {
		fields["steps"] = steps
	}
	return json.MarshalIndent(fields, "", "  ")
}

// decodeCard is a Card file as the JSON object it is, with numbers kept as they were written. A file that
// is not an object is named as the problem, because that is what the human has to go and fix.
func decodeCard(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("card file is not a JSON object: %w", err)
	}
	if fields == nil {
		fields = map[string]any{}
	}
	return fields, nil
}

// applyComment files one Comment on a Card: it takes the next free ordinal, or corrects the one it names.
// The correction stands only while that Comment is the Card's last word from a hand, and only when it is an
// agent's from the same session - a hand-written Comment is the human's to edit, and an agent's own word that
// a human has already answered is corrected by filing the next Comment beside that answer. An ordinal nothing
// holds and the Card would not issue is refused rather than filed, so no Comment lands in a gap.
//
// Like every other Write, this one stamps the Card with the moment its Comment carries; rewrite does that
// stamping, and only when the Comment changed what the Card says.
func applyComment(fields map[string]any, c Comment) error {
	existing, err := commentList(fields["comments"])
	if err != nil {
		return err
	}
	next := nextCommentID(existing)
	// The Card already says this. Filing it again changes nothing, so the fields are left exactly as they were
	// and rewrite answers Changed false - which is what lets the agent's door say "already says that" without
	// reading the Card a second time. A correction names an ordinal and is answered by the
	// rules below, even when another Comment already carries these words.
	if c.ID == 0 {
		for _, e := range existing {
			if e.Text == c.Text {
				return nil
			}
		}
	}
	if c.ID == 0 {
		c.ID = next
	}
	if c.ID == next {
		fields["comments"] = append(existing, c)
		return nil
	}
	for i, e := range existing {
		if e.ID != c.ID {
			continue
		}
		if e.By != byAgent {
			return conflict("Comment %d on this card was written by hand; the tool does not rewrite it", c.ID)
		}
		if e.Session != c.Session {
			return conflict("Comment %d on this card belongs to session %q, not to %q", c.ID, e.Session, c.Session)
		}
		for _, later := range existing[i+1:] {
			if later.By == byHuman {
				return fmt.Errorf("%w: Comment %d on this Card was written by hand after it; file a new Comment instead of rewriting Comment %d", ErrCommentSuperseded, later.ID, c.ID)
			}
		}
		existing[i] = c
		fields["comments"] = existing
		return nil
	}
	return fmt.Errorf("%w: the next one on this Card is #%d", ErrCommentOrdinal, next)
}

// commentList reads whatever the file's comments value is into Comments. Re-encoding through JSON keeps
// this small; a hand-broken value comes back as an error naming the offending JSON.
func commentList(v any) ([]Comment, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var list []Comment
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the comments on this Card are not a list of Comments: %w", err)
	}
	return list, nil
}

// stepList reads whatever the file's steps value is into Steps, the same way commentList reads a hand's
// Comments; a hand-broken value comes back as an error naming the offending JSON.
func stepList(v any) ([]Step, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var list []Step
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("the steps on this Card are not a list of steps: %w", err)
	}
	return list, nil
}

// issuedSteps is the highest step ordinal a Card has issued, read off the JSON fields a Write is deciding on
// rather than off the struct. A Card that recorded none has issued as many numbers as it holds steps, and one
// whose record a hand wrote as a negative is a Card that has issued none of its own.
func issuedSteps(fields map[string]any, steps []Step) int {
	last := 0
	switch v := fields["steps_issued"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			last = int(n)
		}
	case float64:
		if v > 0 {
			last = int(v)
		}
	case int:
		if v > 0 {
			last = v
		}
	}
	for _, s := range steps {
		if s.ID > last {
			last = s.ID
		}
	}
	return last
}
