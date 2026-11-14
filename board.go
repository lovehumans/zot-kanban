package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// A Card's State is its directory; its identity is the filename stem and never
// changes when it moves. Everything is re-read from disk on every use, because cards are
// git-tracked files that an agent, a text editor or git may write without going through us.
//
// board.go holds the Board itself and the verbs that answer for it, together with the one mutex
// they take. The seams they call sit beside it: the Card and its parts in card.go, the
// gates in gate.go, the refusals in refusal.go, the read off disk in listing.go, the file names and
// their publishing in files.go, the read-state in seen.go.

const (
	boardDir   = ".kanban"
	cardExt    = ".json"
	orderStep  = 50 // orders are assigned with gaps; a drag writes into a gap
	idLayout   = "2006-01-02T1504"
	priorityHi = "high"
	priorityLo = "low"
	priorityMi = "mid"
	urlFile    = "url" // this session's board address; volatile, so not git-tracked

	// unseenFile is the read-state file under .kanban: the ids of Agent-filed Cards no one has opened yet.
	// It is volatile the way the board address is, so it is gitignored rather than committed.
	// unseenCommentsFile holds the ids of Cards carrying a Comment no one has opened since an Agent filed it,
	// the same shape and the same gitignored status.
	unseenFile         = "unseen"
	unseenCommentsFile = "unseen_comments"

	// trashName is the directory under .kanban a deleted Card's file moves to. It is not a
	// Column: it is not in Columns, nothing polls it, and a Card in it is not on the board.
	trashName   = "trash"
	byHuman     = "human"
	byAgent     = "agent"
	FirstColumn = "inbox"
	DoneColumn  = "done"
	DoingColumn = "doing" // the Column a Claim is recorded against

	// ClosedColumn is finished work kept: .kanban/closed/, a directory the Board walks and reads but renders
	// only when the view asks for it. It is a Column to a reader and not to a writer - nothing
	// Moves into it, and only a human hand closes a Card or reopens one.
	ClosedColumn = "closed"

	// writeTempPattern is the name a Card is written under before it is linked into place. It does
	// not end in cardExt on purpose, so a column scan never mistakes it for a Card.
	writeTempPattern = ".zot-kanban-write-*"
)

// Columns in the order they are drawn. backlog is the human's and never appears in an
// agent-facing listing; done is only reachable with a commit.
var Columns = []string{"backlog", "inbox", "ready", "doing", "blocked", "done"}

// walkedColumns is what Load reads off disk: every Column the board renders, plus closed, which it reads
// without rendering until a view asks. The walk, the stamps that catch a Card moving mid-walk, filesHolding's
// search and sortBoard's column order all read this one list, so a closed Card is a Card the board knows:
// its id is already among the ones Add refuses to issue a second time, and a stem held in both done and
// closed is the same twin the board already refuses to read over.
var walkedColumns = append(append([]string{}, Columns...), ClosedColumn)

// PlaceEnd is the one place a drag can name that is not a Card: after the last Card this Column displays.
// No Card id can read as it, because an id begins with its timestamp.
const PlaceEnd = "end"

// WorkingColumns is what an agent-facing read defaults to.
var WorkingColumns = []string{"inbox", "ready", "doing", "blocked"}

func validColumn(col string) bool {
	for _, c := range Columns {
		if c == col {
			return true
		}
	}
	return false
}

// Board is one project's board: a directory of column directories.
//
// mu is this process's one write at a time. Nearly every Card write reads before it decides what to write:
// Add reads the board to pick a name and an Order, Move reads done to pick the Order a Card arriving there
// takes and reads the Card to decide whether a claim refuses it, an edit or a Comment reads the file it is
// about to rewrite. A second writer landing inside that window does not lose a sighting the way a scan loses a
// Card in transit; it lands in the files and stays - two Cards holding one Order, a claim stamped over by
// another session, a Comment patched out of the file by a write that never read it. So each read-then-write
// sequence the board does holds this lock for the whole of it.
//
// What it does not reach is a writer outside this process: git, a human's editor and a second zot session keep
// their own hands on these files, and the board answers them the way it always has - by re-reading, by
// refusing a name it did not read, and by naming the two Cards that hold one id.
type Board struct {
	Root string
	mu   sync.Mutex
}

// NewBoard points at <project>/.kanban. The project is wherever the caller is standing, which for
// the extension is the CWD zot reported in hello_ack.
func NewBoard(project string) *Board {
	return &Board{Root: filepath.Join(project, boardDir)}
}

// Load re-reads every Card from disk, under the same lock a Card write holds: a listing that cuts through a
// write is that same window seen from the other side. Use load where this Board's own write already holds it.
//
// What Load answers is the board a reader sees, so it is where the readings a Card face is built from are
// joined to the Cards: the read-state of .kanban/unseen and .kanban/unseen_comments - a Card a hand wrote by
// other means carries no Origin all the same, and gets no mark - and whether this board has more than one
// holder to name.
func (b *Board) Load() ([]Card, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cards, err := b.load()
	if err != nil {
		return nil, err
	}
	unseen, err := b.readUnseen(b.unseenPath())
	if err != nil {
		return nil, err
	}
	unseenComments, err := b.readUnseen(b.unseenCommentsPath())
	if err != nil {
		return nil, err
	}
	holders := holdersNamed(cards)
	for i := range cards {
		cards[i].Unseen = unseen[cards[i].ID]
		cards[i].UnseenComments = unseenComments[cards[i].ID]
		cards[i].NamesHolder = holders
	}
	return cards, nil
}

// A ReadRequest names what a read asks for: the one Column to list, or the one Card to answer. Naming both is
// a refusal the Board answers, not a choice the door makes about which question it will ask.
type ReadRequest struct {
	Column string
	Card   string
}

// Read is a read's answer, so that a door holds no read rules of its own: Columns are the Columns it grouped
// by, in the order the answer says them; Cards are the Cards in those Columns, or the one Card when the read
// named an id. Path is set only for that one Card: the file its name is written in, which only the
// Board knows, because only the Board knows which directory a Column name is a directory of.
type Read struct {
	Columns []string
	Cards   []Card
	Path    string
}

// Read is the Board's answer to a read. Naming no Column walks the four Columns an Agent works;
// naming one walks that one alone, and closed is among the names a read may use because closed is readable
// and not movable. Naming an id answers that Card out of whichever Column holds it - the Column is
// the fact an id copied off the board cannot carry - and asks it the done question a listing asks it.
func (b *Board) Read(req ReadRequest) (Read, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	column := strings.TrimSpace(req.Column)
	id := strings.TrimSpace(req.Card)
	if id != "" && column != "" {
		return Read{}, badInput("%q names one Card and %q names one Column: name one of them", id, column)
	}
	if id != "" {
		col, path, raw, err := b.cardFile(id)
		if err != nil {
			return Read{}, err
		}
		card, err := readCard(raw, col, id, time.Now())
		if err != nil {
			return Read{}, err
		}
		b.markContradiction(&card)
		return Read{Columns: []string{col}, Cards: []Card{card}, Path: path}, nil
	}
	columns, err := readColumns(column)
	if err != nil {
		return Read{}, err
	}
	cards, err := b.load()
	if err != nil {
		return Read{}, err
	}
	wanted := make(map[string]bool, len(columns))
	for _, col := range columns {
		wanted[col] = true
	}
	// load has already put every Card in its Column's place and that Column's display order, and a read's
	// Columns are a run of the walk in the order the walk lists them, so keeping the ones wanted keeps both.
	answer := Read{Columns: columns}
	for _, c := range cards {
		if wanted[c.Column] {
			answer.Cards = append(answer.Cards, c)
		}
	}
	return answer, nil
}

func (b *Board) newID(title string, now time.Time, have map[string]bool) string {
	base := now.Format(idLayout) + "-" + slug(title)
	for n := 2; ; n++ {
		id := base
		if have[base] {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		if !have[id] {
			return id
		}
	}
}

// Add files one Card the way a human files one from the board's composer strip; AddAsAgent files one the way
// an Agent files it with the kanban tool. Which hand captured a Card is its Origin, and it is what the board's
// new mark is built from, so the two doors name themselves rather than passing a string.
//
// Both write into a Column at the end of that Column's Order - the top of it, in the two Columns the board
// reads newest-first.
//
// The name it picks comes from a board read that is not atomic, so a Card filed by another writer can hold
// that name by the time this one is written. save refuses to replace a file it did not read, so the filing
// marks the name as held and takes the next one: two Agents capturing the same idea in the same minute file
// two Cards, rather than one landing on top of the other.
func (b *Board) Add(column, title, body, priority string, now time.Time) (Card, error) {
	return b.add(column, title, body, priority, byHuman, now)
}

// AddAsAgent files one Card as an Agent's filing and records it as unseen, which is what lights the board's
// mark under it and keeps it lit until somebody opens the Card.
func (b *Board) AddAsAgent(column, title, body, priority string, now time.Time) (Card, error) {
	return b.add(column, title, body, priority, byAgent, now)
}

func (b *Board) add(column, title, body, priority, origin string, now time.Time) (Card, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !validColumn(column) {
		return Card{}, badInput("unknown column %q (want one of %s)", column, strings.Join(Columns, ", "))
	}
	if origin != byAgent && origin != byHuman {
		return Card{}, badInput("a Card needs an origin: %s or %s", byHuman, byAgent)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Card{}, badInput("a card needs a title")
	}

	existing, err := b.load()
	if err != nil {
		return Card{}, err
	}
	held := map[string]bool{}
	for _, c := range existing {
		held[c.ID] = true
	}
	// A name a deleted Card still holds in trash counts as held too: an id stays out of circulation while
	// its file is there, so a Card filed now cannot end up answering to the same name as one deleted. A
	// closed Card holds its id the same way.
	trashed, err := b.trashedIDs()
	if err != nil {
		return Card{}, err
	}
	for id := range trashed {
		held[id] = true
	}
	closed, err := b.closedIDs()
	if err != nil {
		return Card{}, err
	}
	for id := range closed {
		held[id] = true
	}
	c := Card{
		Title: title, Body: strings.TrimSpace(body), Priority: priority, Origin: origin,
		Order: maxOrderIn(existing, column) + orderStep, Created: now, Updated: now, Column: column,
	}
	c.normalize(now)

	for attempt := 0; attempt < addNameRetries; attempt++ {
		c.ID = b.newID(title, now, held)
		err := b.save(c)
		if err == nil {
			// The Card is on the board; only an Agent's filing is held unseen. A mark that would not write
			// does not undo the filing, so the report names which of the two happened.
			if origin == byAgent {
				if err := b.markUnseen(b.unseenPath(), c.ID); err != nil {
					return c, fmt.Errorf("card %s filed, but %w", c.ID, err)
				}
			}
			return c, nil
		}
		if !errors.Is(err, ErrNameTaken) {
			return Card{}, err
		}
		held[c.ID] = true // somebody filed this name while this Add was deciding; take the next one
	}
	return Card{}, conflict("add card %q to %s: %w - %d names were held already while this add was filing it",
		title, column, ErrNameTaken, addNameRetries)
}

// maxOrderIn is the end of one Column's Order: the highest Order among its Cards, 0 when it has none. A
// Column's Order is its own sequence, and a Column may be empty; 0 is below both of an Order's accepted
// states.
func maxOrderIn(cards []Card, column string) int {
	highest := 0
	for _, c := range cards {
		if c.Column == column && c.Order > highest {
			highest = c.Order
		}
	}
	return highest
}

// addNameRetries is how many names Add will work through before it says the board is being filed into too
// fast for it. Each name it finds held is one Card somebody else filed at the same moment, so this is room
// for that many writers at once, not a limit on the size of a board.
const addNameRetries = 50

// Move carries a Card to another Column by renaming its file: the id, the Order and the Comments
// travel with it, and git reads it as a rename. done is reachable only with the sha of
// the commit that carries the change, which the Agent that made that commit supplies, and it is left by no
// door at all: the Column gate is asked before anything is renamed.
// A Card arriving in done is the one move that files it at the end of that Column's Order, because done is
// read from that end.
//
// A move is two things the board's one door performs together: the Card travels to another Column, and it
// takes a place in that Column's display. A mover that names a place - what a drag does, by naming the Card
// its dotted line pointed at, or PlaceEnd for after that Column's last Card - puts the Card there. A mover
// that names none keeps the Order the Card already had, which is what a tool move does and what
// dropping a Card onto a Column's heading does.
//
// A Claim is what this rename writes when an Agent asks for a move into doing, and what it refuses an
// Agent's other moves against: a Card one session holds is not taken by another, and the only
// ways a claim ends are the holder filing it done or a human drag taking it out of doing. A drag that sorts
// a Card within the Column it is in is not such a taking: it leaves the Claim where it was.
func (b *Board) Move(cardID, column, commit string, now time.Time, by Mover) (Write, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	commit = strings.TrimSpace(commit)
	// The Card is read first, because the gate answers about a Card: which Column it sits in is half the question.
	col, path, raw, err := b.cardFile(cardID)
	if err != nil {
		return Write{}, err
	}
	if err := columnGate(cardID, col, column, commit, by); err != nil {
		return Write{}, err
	}
	held, err := claimGate(raw, cardID, by)
	if err != nil {
		return Write{}, err
	}
	// Nothing is renamed before the file is known to be a Card: a rename is the one part of a Move a later
	// read cannot undo, and a Card whose bytes a hand broke is refused as the board failing at its own job
	// rather than half-moved and then reported. What is decoded here is the JSON object this file is,
	// which is also how a field this program does not know about survives the move.
	fields, err := decodeCard(raw)
	if err != nil {
		return Write{}, fmt.Errorf("card %s: %w", cardID, err)
	}
	if col == column {
		if by.Above == cardID {
			// A line drawn on the Card's own gap: the drag ended where it started, so nothing moved and
			// nothing is written.
			card, err := readCard(raw, col, cardID, now)
			return Write{Card: card, From: col}, err
		}
		if by.Above != "" {
			// A drop inside the Column a Card already sits in is a sort: the file is not renamed, and the one
			// field that changes is the Order that says where it sits. The Claim is left alone, because putting
			// a Card in place says nothing about who is working it. Whether the place the drag asked for is one
			// the Card already has is rewrite's to answer, so a drag dropped back on its own gap writes nothing.
			place, err := b.placement(cardID, col, by.Above, now)
			if err != nil {
				return Write{}, err
			}
			return rewrite(col, cardID, path, raw, now, func(fields map[string]any) error {
				fields["order"] = place
				return nil
			})
		}
		if column == DoingColumn && by.Session != "" && held == nil {
			// A Card sitting in doing that no session holds - a human dragged it there, or typed the file - is
			// one this Agent is taking up. Nothing is renamed, but who is working it is worth recording,
			// or the Column the guard exists for would be open to every session at once.
			return rewrite(DoingColumn, cardID, path, raw, now, func(fields map[string]any) error {
				fields["claim"] = map[string]any{"session": by.Session, "at": now.Format(time.RFC3339)}
				return nil
			})
		}
		// A Card moved to where it already is changes nothing, so a retry of a move cannot corrupt one.
		card, err := readCard(raw, col, cardID, now)
		return Write{Card: card, From: col}, err
	}
	to := b.cardPath(column, cardID)
	// arrival is the Order a Card takes when the move files it into done; 0 for a move into any other Column,
	// where a Card keeps the Order the human stacked it at.
	var arrival int
	if by.Above != "" && column == DoneColumn {
		return Write{}, badInput("card %s: %s is filed at the end of its Order and is not sorted by hand", cardID, DoneColumn)
	}
	if by.Above != "" {
		// The place has to be settled before anything is renamed, so that a drag pointing at a Card this
		// Column does not hold refuses without moving the Card at all.
		place, err := b.placement(cardID, column, by.Above, now)
		if err != nil {
			return Write{}, err
		}
		arrival = place
	}
	if column == DoneColumn {
		// The name has to be one the repository behind the board answers to: a Card filed done is
		// reopened by reverting the commit it names, so one naming a commit nothing has can never be reopened.
		// It is asked after the claim, because a Card another session holds is the fact a human has to act on
		// and a sha is not, and before the rename, so nothing is ever filed on a promise.
		if err := b.commitIsReal(commit); err != nil {
			return Write{}, fmt.Errorf("card %s: %w", cardID, err)
		}
		// The done move is the one that changes a Card's Order. Done is read from the end of its Order,
		// so a Card arriving there is filed at that end: the close-out reads as the latest thing
		// done rather than under everything filed before it, and its Order follows the moment it reached done
		// instead of the Column it came from.
		done, err := b.load()
		if err != nil {
			return Write{}, err
		}
		arrival = maxOrderIn(done, DoneColumn) + orderStep
	}
	// The decisions the mover asked for, made on the object before any name of it is written to.
	if commit != "" {
		fields["commit"] = commit
	}
	if arrival != 0 {
		fields["order"] = arrival
	}
	if column == DoingColumn && by.Session != "" {
		// An Agent picking a Card up records who picked it up.
		fields["claim"] = map[string]any{"session": by.Session, "at": now.Format(time.RFC3339)}
	}
	if col == DoingColumn && column != DoneColumn {
		// Out of doing the claim goes with it, whoever asked: the holder put the Card down. done is
		// the one Column that keeps the record, because there it says who did the work.
		delete(fields, "claim")
	}
	if err := travel(cardID, path, to); err != nil {
		return Write{}, err
	}
	// The travel is a Write into the Column asked for, and From is the Column the Card was read out of - the
	// sentence needs both, and only the Board knows them together.
	written, err := stampCard(column, cardID, to, fields, now)
	written.From = col
	return written, err
}

// placement is the Order that puts a Card at a named place in a Column's display: above the Card its mover
// named, or after the last Card the Column displays. Between two neighbours it takes their midpoint; past an
// end it takes one orderStep beyond the Card it lands beside, in the direction that Column's display runs -
// upwards for a Column read from the front, downwards for one read from the end.
//
// When no whole number is left to take - two neighbours one apart have no midpoint of their own, and an end
// insert would land at zero or below - the whole Column is repacked at orderStep gaps and the same place is
// taken again. That is rare, and it is the only case where a Card moves whose drag nobody made: only the
// Cards whose numbers actually moved are rewritten, and each one is stamped as changed: its Order moved, and a
// Write that changes something says when it did.
//
// Move holds the board's lock around this, which is what makes "one write at a time inside the board's
// process" hold for the neighbours as well as for the Card being dragged.
func (b *Board) placement(cardID, column, above string, now time.Time) (int, error) {
	cards, err := b.readColumn(column)
	if err != nil {
		return 0, err
	}
	var list []listed
	for _, c := range cards {
		if c.ID != cardID {
			list = append(list, c)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return displayBefore(list[i].Card, list[j].Card) }) // the display, top to bottom, not the number order

	at := len(list)
	switch above {
	case PlaceEnd:
	case "":
		return 0, badInput("card %s: no Card was named to land above", cardID)
	default:
		at = -1
		for i, c := range list {
			if c.ID == above {
				at = i
				break
			}
		}
		if at < 0 {
			return 0, conflict("card %s: %s holds no card %q to land above", cardID, column, above)
		}
	}
	if len(list) == 0 {
		return orderStep, nil
	}

	// outward is the step that goes past the top of the display. A Column read from the front displays its
	// lowest Order first, so a place above its first Card is a lower number; a Column read from its end
	// displays its highest Order first, so a place above its first Card is a higher one.
	outward := -orderStep
	if NewestFirst[column] {
		outward = orderStep
	}
	switch at {
	case 0:
		if o := list[0].Order + outward; o >= 1 {
			return o, nil
		}
	case len(list):
		if o := list[len(list)-1].Order - outward; o >= 1 {
			return o, nil
		}
	default:
		prior, next := list[at-1].Order, list[at].Order
		if o := (prior + next) / 2; o >= 1 && abs(prior-next) >= 2 {
			return o, nil
		}
	}

	// No room left between two Cards. Repack the whole Column at orderStep gaps, the Card being dragged at
	// the place it was asked for, and write the neighbours whose numbers moved.
	seq := make([]listed, 0, len(list)+1)
	seq = append(seq, list[:at]...)
	// The place being asked for, stood in by its id: only its rank in the sequence matters here, and the Card
	// itself is not in the listing, which read the directory it is already in.
	seq = append(seq, listed{Card: Card{ID: cardID, Column: column}})
	seq = append(seq, list[at:]...)
	rank := make([]int, len(seq)) // how far from the top of the display each place is
	for i := range seq {
		rank[i] = i + 1
		if NewestFirst[column] {
			// The top of this display is the highest Order, so the first place takes the largest number.
			rank[i] = len(seq) - i
		}
	}
	for i, c := range list {
		want := rank[i] * orderStep
		if i >= at {
			want = rank[i+1] * orderStep
		}
		// rewrite answers whether this neighbour needed the number at all, so a Card whose place the drag
		// did not change is neither written nor redated. The bytes it rewrites are the ones this listing read.
		if _, err := rewrite(column, c.ID, c.path, c.raw, now, func(fields map[string]any) error {
			fields["order"] = want
			return nil
		}); err != nil {
			return 0, err
		}
	}
	return rank[at] * orderStep, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// CardEdit is one Card's editable half. A field that is nil is a field the request did not name, which is
// what lets a form that carries only a title leave the body, the priority and the order alone. Column is
// deliberately absent: a Card's column is its directory, and Move is the only thing that renames a file.
type CardEdit struct {
	Title    *string
	Body     *string
	Priority *string
	Order    *int
}

// Edit changes what one Card says, in the Column it is already in, and keeps what the request does not
// name: its Comments, its id and filename, and any field this program does not know about.
// A title edit does not rename the Card, so git reads it as a content change and an id a human wrote in
// a Comment still resolves.
//
// A Card already in done is editable: what is refused is a Card *reaching* done without the
// sha of the commit that carries the change, and editing the text of one that is there moves nothing.
//
// rewrite is what an edit is made through: the read, the patch and the write share one window, and Board's
// lock is what makes that window one write at a time in this process: a Comment another door files here is
// filed after this patch, not under it. A writer outside the process is not held by it, and its Comment is
// still the one this patch never read - which is why the board re-reads everything on every request.
func (b *Board) Edit(cardID string, edit CardEdit, now time.Time) (Write, error) {
	return b.edit(cardID, edit, now, false)
}

// EditAsAgent is the Agent's door onto the same change, with the two refusals a hand is never given: a Card a
// hand captured is that hand's to reword, and a Card sitting in closed is not the Agent's to touch. Which hand
// filed a Card is its Origin, recorded at filing and never edited, so a Card the Agent captured stays the
// Agent's to reword after somebody has opened it: the board's new mark says who has read a Card, and this rule
// is about whose words they are.
func (b *Board) EditAsAgent(cardID string, edit CardEdit, now time.Time) (Write, error) {
	return b.edit(cardID, edit, now, true)
}

// edit is what both doors write. byAgent is the fact that the door asking is the Agent's tool and not a hand on
// the board - the same fact Mover.Agent carries for a move - and it brings the two refusals only that door has.
func (b *Board) edit(cardID string, edit CardEdit, now time.Time, byAgent bool) (Write, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if edit.Title == nil && edit.Body == nil && edit.Priority == nil && edit.Order == nil {
		return Write{}, badInput("card %s: nothing to change (name one of title, body, priority, order)", cardID)
	}
	if edit.Title != nil && strings.TrimSpace(*edit.Title) == "" {
		return Write{}, badInput("a card needs a title")
	}
	if edit.Priority != nil {
		switch *edit.Priority {
		case priorityHi, priorityMi, priorityLo:
		default:
			return Write{}, badInput("priority %q: want one of %s, %s, %s", *edit.Priority, priorityHi, priorityMi, priorityLo)
		}
	}
	if edit.Order != nil && *edit.Order <= 0 {
		return Write{}, badInput("order %d: a Card's order is a positive number", *edit.Order)
	}
	col, path, raw, err := b.cardFile(cardID)
	if err != nil {
		return Write{}, err
	}
	if byAgent {
		if err := agentEditGate(cardID, col, raw); err != nil {
			return Write{}, err
		}
	}
	// An edit lands in the Column the Card is already in, and it answers with the Write: whether the bytes
	// changed is the caller's to say anything about, and this is where it is known.
	written, err := rewrite(col, cardID, path, raw, now, func(fields map[string]any) error {
		if edit.Title != nil {
			fields["title"] = strings.TrimSpace(*edit.Title)
		}
		if edit.Body != nil {
			// An empty body is no body, and Add does not write one either; deleting the key keeps the file
			// the shape a Card written here always has.
			if body := strings.TrimSpace(*edit.Body); body != "" {
				fields["body"] = body
			} else {
				delete(fields, "body")
			}
		}
		if edit.Priority != nil {
			fields["priority"] = *edit.Priority
		}
		if edit.Order != nil {
			fields["order"] = *edit.Order
		}
		return nil
	})
	return written, err
}

// What a step Write did, which is the fact the verb's sentence is built from.
const (
	stepFiled       = "filed"
	stepMarked      = "marked"
	stepUnmarked    = "unmarked"
	stepErased      = "erased"
	stepAlreadyDone = "already done"
	stepAlreadyOpen = "already open"
	stepAlreadyFile = "already filed"
)

// StepRequest is one call on the step door: the Card, the session asking, and either the name of the step to
// file or the ordinal of the step to mark, undo or erase.
type StepRequest struct {
	Card    string
	Session string
	Text    string
	Ordinal int
	Op      string
}

// StepWrite is what the step door answers: the Write, and the step it touched with what it did to that step.
// The two are not readable off the Card afterwards - an erased step is no longer on it, a Card that changed
// nowhere says only that nothing moved - and the door hands them over the way FileComment hands over the
// Comment it filed.
type StepWrite struct {
	Write
	Step Step
	Did  string
}

// Step writes steps on a Card through the door a hand reaches: without the refusals only the Agent's door has - the
// Column and Claim gates that keep an Agent's steps on the Card it is working - but with the one the Agent's door
// does not carry, that a Card a session is holding in doing is not a browser's to step on (handStepGate).
// The modal's boxes write through here; the Card's own file is the other hand's door and answers no gate at all,
// which is why a hand can write nine steps into a file the door would refuse to file one at a time.
func (b *Board) Step(req StepRequest, now time.Time) (StepWrite, error) {
	return b.step(req, now, false)
}

// StepAsAgent is the Agent's door onto the same writes, with the gate a hand is never given: a Card's steps
// belong to the session holding that Card in doing. The gate reads the Claim and not the Origin, because a step
// is what a session says about the work it is doing on any Card - including one a human captured - whereas the
// Origin gate is about whose words a Card is.
func (b *Board) StepAsAgent(req StepRequest, now time.Time) (StepWrite, error) {
	return b.step(req, now, true)
}

// step is what both doors write: the arguments settled first, then the Card read, then the gate asked inside
// the same locked window the write lands in, then one decision applied to the Card's JSON by
// rewrite - which is where a step that changes nothing is known to change nothing and is written nowhere.
//
// A step Write lights no mark and clears none: the new mark and the comment mark say *nobody has read this since
// an Agent said something*, and only the Comment door lights one. A Card whose steps an
// Agent filed is not a Card a human has to re-open to see: its step count is printed on its face.
func (b *Board) step(req StepRequest, now time.Time, byAgent bool) (StepWrite, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch req.Op {
	case "", stepUndo, stepErase:
	default:
		return StepWrite{}, badInput(`step: "op" is %s or %s, and %q is neither`, stepUndo, stepErase, req.Op)
	}
	text := strings.TrimSpace(req.Text)
	if req.Op != "" && req.Ordinal == 0 {
		return StepWrite{}, badInput(`step %s: needs "ordinal", the ordinal of the step on this Card to %s`, req.Op, req.Op)
	}
	if req.Ordinal < 0 {
		return StepWrite{}, badInput("step ordinal %d: a step's ordinal is a whole number", req.Ordinal)
	}
	if text != "" && req.Ordinal != 0 {
		return StepWrite{}, badInput("card %s: %s", req.Card, ruleStepReword)
	}
	if text == "" && req.Ordinal == 0 {
		return StepWrite{}, badInput(`step: name "text" for a step to file or "ordinal" for the step to mark`)
	}
	if text != "" {
		if n := utf8.RuneCountInString(text); n > stepNameLimit {
			return StepWrite{}, badInput("a step's name is at most %d characters, and this one is %d: a step names one small task",
				stepNameLimit, n)
		}
		if strings.ContainsAny(text, "\n\r") {
			return StepWrite{}, badInput("a step's name is one line: %q carries a newline", text)
		}
	}
	col, path, raw, err := b.cardFile(req.Card)
	if err != nil {
		return StepWrite{}, err
	}
	// A Card in closed answers no step from either door, so this one sits in the door every caller goes through:
	// the only way out of closed is a hand's reopen, and a browser that had this Card open before it was closed
	// has no Board door its click could reach.
	if col == ClosedColumn {
		return StepWrite{}, conflict("card %s: %s", req.Card, ruleStepClosed)
	}
	if byAgent {
		if err := agentStepGate(req.Card, col, raw, req.Session); err != nil {
			return StepWrite{}, err
		}
	} else if err := handStepGate(req.Card, col, raw); err != nil {
		return StepWrite{}, err
	}
	var touched StepWrite
	written, err := rewrite(col, req.Card, path, raw, now, func(fields map[string]any) error {
		st, did, err := applyStep(fields, req)
		if err != nil {
			// Named by Card, because a Card whose steps the board cannot read is a Card the human has to go and
			// fix.
			return fmt.Errorf("card %s: %w", req.Card, err)
		}
		touched.Step, touched.Did = st, did
		return nil
	})
	if err != nil {
		return StepWrite{}, err
	}
	touched.Write = written
	return touched, nil
}

// applyStep files a step, or marks, un-marks or erases the one a named ordinal holds, and answers with the
// step it touched and what it did to it. Filing the same name twice is the step it already is, and marking one
// already done changes nothing: the Card's fields are left exactly as they were so rewrite writes nothing and
// the door answers "no change". That is what makes "mark a step done the moment it is done" safe to
// obey twice - which is the case a session working from a compacted context arrives at.
//
// The 42-character name and the seven-step cap are refused here and nowhere else, so a Card whose steps a hand
// wrote in its own file keeps all nine of them and every long name in them.
func applyStep(fields map[string]any, req StepRequest) (Step, string, error) {
	steps, err := stepList(fields["steps"])
	if err != nil {
		return Step{}, "", err
	}
	// The decision is made on the numbers the Card is read as, so an ordinal a session read a moment ago is still
	// the step it names after this Write, and the number a filing takes is above every one of them.
	steps, issued := repairStepOrdinals(steps, issuedSteps(fields, steps))
	if text := strings.TrimSpace(req.Text); text != "" {
		for _, s := range steps {
			if s.Name == text {
				return s, stepAlreadyFile, nil
			}
		}
		if len(steps) >= maxLiveSteps {
			return Step{}, "", badInput("a Card holds %d steps at most, and this one already holds %d: a Card needing an eighth step is two Cards",
				maxLiveSteps, len(steps))
		}
		fresh := Step{ID: nextStepID(steps, issued), Name: text}
		fields["steps"] = append(steps, fresh)
		fields["steps_issued"] = fresh.ID
		return fresh, stepFiled, nil
	}
	at := -1
	for i, s := range steps {
		if s.ID == req.Ordinal {
			at = i
		}
	}
	if at < 0 {
		// Bad input rather than a conflict: re-reading the Card cannot make this number one this Card holds,
		// and the refusal names the ordinals it does.
		return Step{}, "", badInput("no step %d on this Card; its steps are %s", req.Ordinal, heldOrdinals(steps))
	}
	switch req.Op {
	case "":
		if steps[at].Done {
			return steps[at], stepAlreadyDone, nil
		}
		steps[at].Done = true
		fields["steps"] = steps
		return steps[at], stepMarked, nil
	case stepUndo:
		if !steps[at].Done {
			return steps[at], stepAlreadyOpen, nil
		}
		steps[at].Done = false
		fields["steps"] = steps
		return steps[at], stepUnmarked, nil
	default: // stepErase
		gone := steps[at]
		kept := append(steps[:at], steps[at+1:]...)
		if len(kept) == 0 {
			delete(fields, "steps")
		} else {
			fields["steps"] = kept
		}
		// The numbers this Card spent outlive the steps that wore them: the record is kept even when the last
		// step is erased, so no later step is handed an ordinal a session already worked under.
		fields["steps_issued"] = issued
		return gone, stepErased, nil
	}
}

// heldOrdinals names the step ordinals a Card holds, for the refusal that says an ordinal is not one of them.
func heldOrdinals(steps []Step) string {
	if len(steps) == 0 {
		return "none"
	}
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, fmt.Sprintf("%d", s.ID))
	}
	return strings.Join(names, ", ")
}

// Delete takes a Card off the board into .kanban/trash/. Nothing there is listed, polled or
// opened, so the Card is gone from the board while its file - and with it its id, its Comments and any field
// this program does not know about - is still there to be restored, by Restore, by a plain mv, or by git for
// a Card that was ever committed.
//
// Delete answers work that has not reached done. A Card in done is never deleted, at any door: finished work
// is closed or reverted with git, and a wrong done is a commit to undo rather than a file to move.
// A Card in closed is never deleted either, because it is the record the closing was for; the door out of
// closed is Reopen. What is left is a hand with a file manager, which no Board door and no Agent is.
func (b *Board) Delete(cardID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	col, _, _, err := b.cardFile(cardID)
	if err != nil {
		return err
	}
	if col == DoneColumn || col == ClosedColumn {
		return conflict("a Card in %s is never deleted: card %s - finished work is closed, not deleted, and a Card in closed is reopened", col, cardID)
	}
	return b.offBoard(cardID, b.trashDir())
}

// Reopen is the door back out of closed: the Card's file moves into done and is filed at the end of that
// Column's Order, so the Card that has to be worked again is the first one a reader of done sees.
// It answers only a Card sitting in closed: a Move into done is the door a commit walks through and
// a reopen names no new commit - the Card already carries the one that filed it there - while Restore is the
// undo of a delete and answers only a Card in Trash. Like closing, it is a human's door alone.
func (b *Board) Reopen(cardID string, now time.Time) (Write, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	col, path, raw, err := b.cardFile(cardID)
	if err != nil {
		return Write{}, err
	}
	if col != ClosedColumn {
		return Write{}, conflict("reopen answers a Card in closed: card %s is in %s", cardID, col)
	}
	// The number is settled while the Card still sits in closed, so maxOrderIn reads the done column the
	// Card is joining rather than the Card itself.
	done, err := b.load()
	if err != nil {
		return Write{}, err
	}
	arrival := maxOrderIn(done, DoneColumn) + orderStep
	to := b.cardPath(DoneColumn, cardID)
	// The Card is known to be a Card before its file is renamed, as in Move.
	fields, err := decodeCard(raw)
	if err != nil {
		return Write{}, fmt.Errorf("card %s: %w", cardID, err)
	}
	fields["order"] = arrival
	if err := travel(cardID, path, to); err != nil {
		return Write{}, err
	}
	// The travel is a Write into done, and From is the closed directory the Card's file left.
	written, err := stampCard(DoneColumn, cardID, to, fields, now)
	written.From = col
	return written, err
}

// Close takes a finished Card off the board into .kanban/closed/, where the board keeps it rather than show
// it. Only a Card in done can be closed, because closed says "this work is over and is kept" while
// the file records no Column of its own - Column is the directory a Card is read out of - so a Card closed
// from anywhere else would come back as finished work it never was. Abandoning a Card that never reached done
// is what delete is for, and Trash is where that Card waits instead.
//
// No Agent door and no drag offers a close: it is the modal's button and the shell's verb, which is the same
// line delete is held on, because a tool that can close can empty a human's done column in one wrong
// call.
//
// A done Card still open on one of its own steps is not closed either. Closing is the door that keeps a Card as
// finished work, and a step the Card wrote and never marked says the work it described is not over - so the
// refusal names the open steps and the modal's box is the door that clears them. Reaching done is a
// different decision and is not refused over a step: done costs the sha of the commit that carries the change,
// and a step is no such evidence in either direction.
func (b *Board) Close(cardID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	col, _, raw, err := b.cardFile(cardID)
	if err != nil {
		return err
	}
	if col != DoneColumn {
		return conflict("%s: card %s is in %s", ruleCloseFrom, cardID, col)
	}
	open, err := openStepsOf(raw)
	if err != nil {
		return fmt.Errorf("card %s: %w", cardID, err)
	}
	if len(open) > 0 {
		return conflict("card %s: %s - %s", cardID, openStepWords(open), ruleCloseSteps)
	}
	return b.offBoard(cardID, b.closedDir())
}

// CloseAllDone is the bulk door over finished work: every done Card whose done-ness holds up is closed, and a
// Card that contradicts its own commit stays in done, where the board prints the finding and the singular close
// is still the door a human can take it through. A Card still open on one of its own steps
// is left there too, in the same red the face paints it in: the bulk door checks what a human is looking at when
// it moves a column, and here it would be keeping as finished work a Card whose own steps say otherwise.
//
// It is the CLI's verb and no one else's: no web request and no Agent action asks for it, which is why this
// repository's own done column is emptied by a human typing it and not by a session.
func (b *Board) CloseAllDone() ([]Card, []Card, error) {
	cards, err := b.Load()
	if err != nil {
		return nil, nil, err
	}
	var closed, skipped []Card
	for _, c := range cards {
		if c.Column != DoneColumn {
			continue
		}
		if c.Contradiction != "" || len(unmetSteps(c)) > 0 {
			skipped = append(skipped, c)
			continue
		}
		if err := b.Close(c.ID); err != nil {
			return closed, skipped, err
		}
		closed = append(closed, c)
	}
	return closed, skipped, nil
}

// offBoard moves a Card's file out of the Column holding it into dest - trash or closed - and takes the
// Card's read-state marks with it. Nothing is rewritten on the way, so the bytes the Card was committed with
// are the bytes that land in dest, which is what lets git read the move as a rename rather than a deletion
// plus an addition. Re-marshalling the Card on the way would be that failure by another door, so
// the file's own bytes are what travel.
func (b *Board) offBoard(cardID, dest string) error {
	_, path, _, err := b.cardFile(cardID)
	if err != nil {
		return err
	}
	if err := travel(cardID, path, filepath.Join(dest, filepath.Base(path))); err != nil {
		return err
	}
	// A Card off the board is not a Card held unseen: the read-state files keep only what is still on the
	// board, so a close and a delete both take their marks with them.
	for _, state := range []string{b.unseenPath(), b.unseenCommentsPath()} {
		if err := b.forgetUnseen(state, cardID); err != nil {
			return err
		}
	}
	return nil
}

// Restore puts a deleted Card back into a Column, keeping its id, its Comments and its file. It is the undo
// Delete exists for; the same thing by hand is mv .kanban/trash/<file>.json .kanban/<column>/, and git
// history is the undo for a Card that was committed before it was deleted.
func (b *Board) Restore(cardID, column string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !validColumn(column) {
		return badInput("unknown column %q (want one of %s)", column, strings.Join(Columns, ", "))
	}
	// A Card that is on the board is not restored, and an id two Cards hold is refused here as everywhere. The
	// locate every door asks answers this one too: the file is in a Column, or in trash, or nowhere.
	found, err := b.filesHolding(cardID)
	if err != nil {
		return err
	}
	var cards, off []held
	for _, h := range found {
		if h.onBoard() {
			cards = append(cards, h)
		} else {
			off = append(off, h)
		}
	}
	if len(cards) > 1 {
		return heldTwice(cardID, pathsOf(cards)...)
	}
	if len(cards) == 1 {
		return conflict("card %s is in %s: only a deleted Card is restored", cardID, cards[0].dir)
	}
	// Only a name that trash holds is restored, and the locate just read trash to answer that.
	if len(off) == 0 {
		return fmt.Errorf("%w %q: no deleted Card by that name in %s", ErrNoCard, cardID, b.trashDir())
	}
	return travel(cardID, b.trashedPath(cardID), b.cardPath(column, cardID))
}

// AddComment files one Comment on a Card, or rewrites the Comment with the given id when that one is
// the agent's own from this session. The id is assigned here, because "starts at 1 and grows
// within that Card" is a fact about the file, not about the caller. Either way the words came through an
// Agent's door, so the Card is held as carrying a Comment nobody has opened.
func (b *Board) AddComment(cardID string, c Comment) (Write, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c.Text = strings.TrimSpace(c.Text)
	if c.Text == "" {
		return Write{}, badInput("a Comment needs text")
	}
	switch c.By {
	case byAgent, byHuman:
	default:
		return Write{}, badInput(`a Comment needs "by": %s or %s`, byHuman, byAgent)
	}
	if c.At.IsZero() {
		// The moment a Comment carries is the moment this Write stamps the Card with, so it is settled before
		// anything is read or written.
		c.At = time.Now()
	}
	// The Card is read before any gate answers, so that a Card a delete took off the board is refused as that
	// Card - naming where its file is and the door that puts it back - rather than as this door's own "not
	// found", the way a read and a move answer it.
	col, path, raw, err := b.cardFile(cardID)
	if err != nil {
		return Write{}, err
	}
	if c.By == byAgent && strings.TrimSpace(c.Session) == "" {
		// The name is what makes a Comment correctable at all: the edit guard compares one Session against
		// another, and an empty one matches another session-less process's empty one, so a Comment from nobody
		// is a Comment nobody may correct.
		return Write{}, badInput("card %s: %w, and an Agent's Comment names the session that wrote it", cardID, ErrNoSession)
	}
	written, err := rewrite(col, cardID, path, raw, c.At, func(fields map[string]any) error {
		if err := applyComment(fields, c); err != nil {
			// Named by id, because a Card whose Comments the board cannot read is a Card the human has to go
			// and fix.
			return fmt.Errorf("card %s: %w", cardID, err)
		}
		return nil
	})
	if err != nil {
		return Write{}, err
	}
	// An Agent's word is a human's to read, so the Comment this door filed lights the Card's comment mark until
	// somebody opens it. A Comment the Card already says lights nothing and is answered without a
	// line: the words were already there to read.
	if written.Changed {
		if err := b.markUnseen(b.unseenCommentsPath(), cardID); err != nil {
			return written, fmt.Errorf("card %s: the Comment was filed, but %w", cardID, err)
		}
	}
	return written, nil
}

// FileComment files one Comment through the board's own door: a human typing in the modal. by is human, at
// is now, and want is the ordinal that Comment means to take (0 means "the next one on this Card", which the
// modal fills in when it renders the form). Naming the ordinal is what makes a submission that arrives twice
// harmless, and so is the text: a Card that already carries these words answers with the Comment it has and
// rewrites nothing, while an ordinal somebody else filled with other words is refused as ErrCommentTaken and
// one past the end of the Card as ErrCommentOrdinal, rather than quietly taking a different number. A Card in
// done takes Comments like any other Card, and the sha that filed it is untouched.
func (b *Board) FileComment(cardID, text string, want int, now time.Time) (Write, Comment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text = strings.TrimSpace(text)
	if text == "" {
		return Write{}, Comment{}, badInput("a Comment needs text")
	}
	if want < 0 {
		return Write{}, Comment{}, badInput("comment id %d: want a whole number", want)
	}
	col, path, raw, err := b.cardFile(cardID)
	if err != nil {
		return Write{}, Comment{}, err
	}
	var record Comment
	written, err := rewrite(col, cardID, path, raw, now, func(fields map[string]any) error {
		comment, err := applyHumanComment(fields, Comment{Text: text, By: byHuman, At: now}, want)
		if err != nil {
			// Named by Column and id, because the refusals that reach this point are ones only a hand at the
			// file can settle.
			return fmt.Errorf("card %s/%s: %w", col, cardID, err)
		}
		record = comment
		return nil
	})
	if err != nil {
		return Write{}, Comment{}, err
	}
	return written, record, nil
}

// CountByColumn reports the card count per column in draw order, in one read. closed comes after every Column
// the board renders, because it is counted with them and shown apart.
func (b *Board) CountByColumn() ([]string, []int, error) {
	cards, err := b.Load()
	if err != nil {
		return nil, nil, err
	}
	return walkedColumns, counts(cards), nil
}

// WriteURL records the address this session's board server bound, so `zot-kanban url` run outside
// a zot session has a candidate to check. It is a hint, not the truth: the truth is what answers.
func (b *Board) WriteURL(url string) error {
	return os.WriteFile(filepath.Join(b.Root, urlFile), []byte(url+"\n"), 0o644)
}

// Seen records that somebody has opened this Card, which is the one thing that clears the marks the board puts
// on it: not age, not a move, and not which browser did it - one store on the board is why a Card opened here
// stops being new everywhere. Opening a Card is reading it, so it clears the mark a new Comment
// wears as well. Opening a Card the board never marked is the same board afterwards, so it is not an
// error; an id that names no Card is.
func (b *Board) Seen(cardID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, _, _, err := b.cardFile(cardID); err != nil {
		return err
	}
	for _, path := range []string{b.unseenPath(), b.unseenCommentsPath()} {
		if err := b.forgetUnseen(path, cardID); err != nil {
			return err
		}
	}
	return nil
}
