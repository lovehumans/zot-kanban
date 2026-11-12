package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Card's State is its directory; its identity is the filename stem and never
// changes when it moves. Everything is re-read from disk on every use, because cards are
// git-tracked files that an agent, a text editor or git may write without going through us.

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

	Column string `json:"-"` // the directory it was found in
	ID     string `json:"-"` // the filename stem

	// Contradiction says why this Card's place in done does not hold up, and is empty when it holds up:
	// a Card filed done with no commit named, or with a name the repository behind the board has
	// no commit of. It is a reading of the Card, never a field of its file, and it is what the board shows
	// instead of pretending a done Card is fine.
	Contradiction string `json:"-"`

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

// slug turns a title into the human-readable half of a card id.
func slug(title string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "card"
	}
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

func validColumn(col string) bool {
	for _, c := range Columns {
		if c == col {
			return true
		}
	}
	return false
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

func (b *Board) colDir(col string) string { return filepath.Join(b.Root, col) }

// trashDir is where a deleted Card's file goes. It sits beside the Column directories under .kanban and is
// deliberately not one of them: Board reads Columns, so nothing in it is listed, counted or opened.
func (b *Board) trashDir() string { return filepath.Join(b.Root, trashName) }

// trashedPath is the name a deleted Card keeps in trash: the same stem it had in a Column.
func (b *Board) trashedPath(id string) string { return filepath.Join(b.trashDir(), id+cardExt) }

// closedDir is where a closed Card's file goes: finished work the board keeps rather than shows. Unlike
// trash it is walked, so its Cards are listed on demand, opened, and asked about the commit they name.
func (b *Board) closedDir() string { return filepath.Join(b.Root, ClosedColumn) }

func (b *Board) closedPath(id string) string { return filepath.Join(b.closedDir(), id+cardExt) }

// cardPath is where a card lives, given the column it is in. Moving a card is rewriting this path,
// which is what lets git mv do the same thing outside this program.
func (b *Board) cardPath(col, id string) string {
	return filepath.Join(b.colDir(col), id+cardExt)
}

func (b *Board) ensure() error {
	for _, col := range Columns {
		if err := os.MkdirAll(b.colDir(col), 0o755); err != nil {
			return fmt.Errorf("create column %s: %w", col, err)
		}
	}
	// The off-board destinations exist from the first look, so a close or a delete never has to create its
	// destination mid-write.
	for _, off := range []string{b.trashDir(), b.closedDir()} {
		if err := os.MkdirAll(off, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", off, err)
		}
	}
	return nil
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

// load is the walk in the two things it does: sight what the Columns hold, and settle that into the Cards the
// board lists. No caching: an out-of-process write must show up on the next request.
func (b *Board) load() ([]Card, error) {
	sighted, err := b.sight()
	if err != nil {
		return nil, err
	}
	cards, err := b.resolveWalk(sighted)
	if err != nil {
		return nil, err
	}
	sortBoard(cards)
	b.markDoneContradictions(cards)
	return cards, nil
}

// sight lists every Column the Board walks, then reads again the ones that moved under the walk.
//
// The mirror of a sighting twice (resolveWalk) is a Card the walk sights in neither Column: it leaves a Column
// the walk has not reached and arrives in one it has already read, and that scan lists nothing. A Card write is
// a link or a rename into the Column that holds it, both of them when a Card moves between Columns, so every
// Column is stamped before the walk and again after it, and a Column whose stamp moved was not steady while
// this walk read the others. sight hands that fact to resteady.
//
// The bracket has to span the whole walk. Stamping a Column just before it is listed and just after it would
// report nothing here: the Card arrived in the first Column after that Column's stamp was taken and left the
// second before that Column's stamp was taken, so no stamp window holds the rename, and only a window that does
// can report it.
func (b *Board) sight() ([]listed, error) {
	before, err := b.columnStamps()
	if err != nil {
		return nil, err
	}
	var sighted []listed
	for _, col := range walkedColumns {
		cards, err := b.readColumn(col)
		if err != nil {
			return nil, err
		}
		sighted = append(sighted, cards...)
	}
	after, err := b.columnStamps()
	if err != nil {
		return nil, err
	}
	added, err := b.resteady(before, after, sighted)
	if err != nil {
		return nil, err
	}
	return append(sighted, added...), nil
}

// resteady is the stamp bracket's rule: the Columns whose stamp moved under the walk are read again, for any
// Card they hold that this walk has not put on the board yet. One re-read is the bound, and a Card still in
// motion when it happens is the next scan's to get right.
func (b *Board) resteady(before, after []time.Time, sighted []listed) ([]listed, error) {
	found := make(map[string]bool, len(sighted)) // the ids this walk has put on the board
	for _, c := range sighted {
		found[c.ID] = true
	}
	var added []listed
	for i, col := range walkedColumns {
		if before[i].Equal(after[i]) {
			continue
		}
		// What a re-read is for is the Card this walk saw nowhere, so it adds only ids it has not put on the
		// board yet. An id already sighted is left to the sighting it got, rather than being sighted a second
		// time in the Column it has since reached: a re-read must not manufacture a Card in transit.
		reread, err := b.readColumn(col)
		if err != nil {
			return nil, err
		}
		for _, c := range reread {
			if found[c.ID] {
				continue
			}
			found[c.ID] = true
			added = append(added, c)
		}
	}
	return added, nil
}

// resolveWalk settles a walk's sightings into the Cards the board lists, one per id.
//
// The walk is not atomic, and a Card can be renamed while it is walking: the listing of the Column it left is
// already in hand by the time the Column it arrived in is read, so one Card can be sighted twice. That is
// settled by asking which of the sighted files still holds it - the same locate a Card door asks before it
// writes - and the sighting of a file that is gone is dropped, so the Card is listed once, in the Column it is
// in now.
//
// A sighting that survives in two Columns is a different thing and is reported: an id is a filename stem,
// and a stem is unique only if whoever named the file made it so, so a hand-written Card or a git
// mv can drop one beside a twin. The board then refuses to read rather than list both as though the id meant
// one of them, because nothing that writes through an id can be trusted to land on the Card the human meant.
func (b *Board) resolveWalk(sighted []listed) ([]Card, error) {
	held := map[string][]int{} // an id to the sightings that hold it
	for i, c := range sighted {
		held[c.ID] = append(held[c.ID], i)
	}

	dropped := map[int]bool{}
	asked := map[string]bool{} // an id is settled once, in the order the walk met its sightings
	for _, c := range sighted {
		id := c.ID
		if len(held[id]) < 2 || asked[id] {
			continue
		}
		asked[id] = true
		// At most one of these sightings can still be a file, or this id is held by two Cards.
		holding, err := b.filesHolding(id)
		if err != nil {
			return nil, err
		}
		there := make(map[string]bool, len(holding))
		for _, h := range holding {
			there[h.path] = true
		}
		var live []int // the sightings that are still a file
		for _, i := range held[id] {
			if there[sighted[i].path] {
				live = append(live, i)
			}
		}
		if len(live) >= 2 {
			holders := make([]string, 0, len(live))
			for _, i := range live {
				holders = append(holders, sighted[i].path)
			}
			return nil, heldTwice(id, holders...)
		}
		// At most one file is there now, so this was one Card moving, not two Cards: keep the sighting of the
		// file that exists (or the last one, if it has moved out of every Column this walk read).
		keep := held[id][len(held[id])-1]
		if len(live) == 1 {
			keep = live[0]
		}
		for _, i := range held[id] {
			if i != keep {
				dropped[i] = true
			}
		}
	}

	cards := make([]Card, 0, len(sighted))
	for i, c := range sighted {
		if !dropped[i] {
			cards = append(cards, c.Card)
		}
	}
	return cards, nil
}

// listed is one Card as a Column's directory held it: the Card the file says it is, where that file sits, and
// the bytes it holds. The bytes come back with the Card because a door that goes on to rewrite what it listed
// - the repack that makes room for a drag - has read them once already, and rewriting them is not a reason to
// read them again.
type listed struct {
	Card
	path string
	raw  []byte
}

// readColumn lists one Column and reads every Card in it, in the order the listing gave them.
func (b *Board) readColumn(col string) ([]listed, error) {
	entries, err := os.ReadDir(b.colDir(col))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read column %s: %w", col, err)
	}
	var cards []listed
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), cardExt) {
			continue
		}
		name := filepath.Join(b.colDir(col), ent.Name())
		raw, err := os.ReadFile(name)
		if err != nil {
			if os.IsNotExist(err) {
				// Another writer moved this Card out from under the scan. The next scan is correct;
				// failing here would lose the add or render that happened to trigger it.
				continue
			}
			return nil, fmt.Errorf("read %s/%s: %w", col, ent.Name(), err)
		}
		var c Card
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("parse %s/%s: %w", col, ent.Name(), err)
		}
		c.Column = col
		c.ID = strings.TrimSuffix(ent.Name(), cardExt)
		c.normalize(time.Now())
		cards = append(cards, listed{Card: c, path: name, raw: raw})
	}
	return cards, nil
}

// columnStamps is every walked directory's own modification time, in walkedColumns order: whether that
// directory has had an
// entry added, removed or renamed since it was last asked. Every Card write publishes into the Column that
// holds it, by a temporary name in that directory and then a link or a rename over its own path, so a Column
// whose stamp moved is one where a Card arrived, left or was rewritten while this walk read the others. A
// Column that does not exist has no stamp to compare and none to change.
func (b *Board) columnStamps() ([]time.Time, error) {
	stamps := make([]time.Time, len(walkedColumns))
	for i, col := range walkedColumns {
		info, err := os.Stat(b.colDir(col))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat column %s: %w", col, err)
		}
		stamps[i] = info.ModTime()
	}
	return stamps, nil
}

// What a Card in done has to name, said the way the board says it.
const (
	noCommitNamed      = "no commit named"
	noSuchCommitFmt    = "names %s, which this repository has no commit of"
	malformedCommitFmt = "names %s, which is not the shape of a commit name"
)

// commitTypes asks git, in one call, what kind of object each name is. It answers one line per name in the
// order they were given: "<oid> <type> <size>" for a name the repository has and "<name> missing" for one it
// does not. One call is what a board needs, because a board is re-read every few seconds and every Card in
// done is asked about each time. A board in a directory git does not know as a repository answers nothing, and
// that is returned as the error rather than read as "no commit anywhere".
func (b *Board) commitTypes(names []string) (map[string]string, error) {
	var stderr strings.Builder
	cmd := exec.Command("git", "-C", b.Root, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	types := map[string]string{}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, name := range names {
		if i < len(lines) {
			if fields := strings.Fields(lines[i]); len(fields) >= 2 {
				types[name] = fields[1]
			}
		}
	}
	if err != nil && len(types) == 0 {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return nil, fmt.Errorf("git at %s says %s", b.Root, reason)
	}
	return types, nil
}

// markDoneContradictions fills in, for every Card it is given that sits in done or in closed, why its
// done-ness does not
// hold up. A Card named with nothing is settled without asking anything; a name is put to git once,
// in one call, and a name that is not the shape of a sha is refused before it is asked, which is also what
// keeps a name from reaching git as an option rather than as a name. A board that cannot ask - no repository,
// no git - says so on each Card that named one, because a done Card whose evidence cannot be checked is not a
// done Card that has been checked.
func (b *Board) markDoneContradictions(cards []Card) {
	var names []string
	asking := map[string]bool{}
	for i, c := range cards {
		if c.Column != DoneColumn && c.Column != ClosedColumn {
			continue
		}
		switch {
		case c.Commit == "":
			cards[i].Contradiction = noCommitNamed
		case !commitShaped(c.Commit):
			cards[i].Contradiction = fmt.Sprintf(malformedCommitFmt, c.Commit)
		case !asking[c.Commit]:
			asking[c.Commit] = true
			names = append(names, c.Commit)
		}
	}
	if len(names) == 0 {
		return
	}
	types, err := b.commitTypes(names)
	for i, c := range cards {
		if (c.Column != DoneColumn && c.Column != ClosedColumn) || !asking[c.Commit] {
			continue
		}
		if types[c.Commit] == "commit" {
			continue
		}
		if err != nil {
			cards[i].Contradiction = fmt.Sprintf("names %s, and %s", c.Commit, err)
			continue
		}
		cards[i].Contradiction = fmt.Sprintf(noSuchCommitFmt, c.Commit)
	}
}

// markContradiction asks a Card on its own the question the board asks of all of them, so a Card shown in the
// modal is marked the way the same Card is marked on the board.
func (b *Board) markContradiction(c *Card) {
	one := []Card{*c}
	b.markDoneContradictions(one)
	*c = one[0]
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

// readColumns is the read's Column gate: the four an Agent works when no Column is named, the named one when
// it is a name the board walks. A read may name closed and a Move may not - closed is reached by a close -
// so this gate's list of names is the walk's own rather than the writer's.
func readColumns(named string) ([]string, error) {
	if named == "" {
		return WorkingColumns, nil
	}
	if named == ClosedColumn || validColumn(named) {
		return []string{named}, nil
	}
	return nil, badInput("unknown column %q (want one of %s)", named, strings.Join(walkedColumns, ", "))
}

// NewestFirst names the Columns the board reads from the end a human works at: inbox, because the Card filed
// there last is the one the human has not looked at yet, and done, because the close-out worth seeing is the
// most recent one, and closed, which carries the Order it had in done and keeps that same sequence.
// Everywhere else a Card's Order is read from the front, the way it was stacked. The Order
// itself is not reversed - Add still writes the next one at the end of its Column, and a drag carries a
// Card's Order into the next Column untouched. Only the reading is.
var NewestFirst = map[string]bool{FirstColumn: true, DoneColumn: true, ClosedColumn: true}

// sortBoard orders cards by column position, then the human's Order, then age, reading a newest-first Column
// from its other end.
func sortBoard(cards []Card) {
	sort.SliceStable(cards, func(i, j int) bool { return displayBefore(cards[i], cards[j]) })
}

// displayBefore is the board's display order, said of two Cards rather than of a whole board: it is what a
// door that needs the order of one Column - the place a drag would land in - reads off the same rule the
// board itself renders by.
func displayBefore(a, b Card) bool {
	if ia, ib := walkedIndex(a.Column), walkedIndex(b.Column); ia != ib {
		return ia < ib
	}
	// One Column at a time here, so the direction is the same for both Cards.
	newest := NewestFirst[a.Column]
	if a.Order != b.Order {
		if newest {
			return a.Order > b.Order
		}
		return a.Order < b.Order
	}
	// Two Cards can share an Order, because Add names the next one off a board read that is not atomic.
	// The tie reads the way the Column does.
	if newest {
		return a.Created.After(b.Created)
	}
	return a.Created.Before(b.Created)
}

// walkedIndex is a walked directory's place in the order the board walks them, so closed sorts after every
// Column the board renders. A directory the board does not walk sorts last.
func walkedIndex(col string) int {
	for i, c := range walkedColumns {
		if c == col {
			return i
		}
	}
	return len(walkedColumns)
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

// save writes one Card that does not exist yet. The Column directory it needs is made by the primitive that
// publishes it, since git does not track empty directories and a fresh clone has none of the six columns in it.
func (b *Board) save(c Card) error {
	// Filed through the same generic object a later patch reads and writes, so the shape is decided once and
	// every write after it names lines instead of recasting the file.
	written, err := json.Marshal(c)
	if err != nil {
		return err
	}
	fields, err := decodeCard(written)
	if err != nil {
		return err
	}
	raw, err := cardBytes(fields)
	if err != nil {
		return err
	}
	return publishCardFile(b.cardPath(c.Column, c.ID), raw)
}

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

// addNameRetries is how many names Add will work through before it says the board is being filed into too
// fast for it. Each name it finds held is one Card somebody else filed at the same moment, so this is room
// for that many writers at once, not a limit on the size of a board.
const addNameRetries = 50

// writeCardTemp lays a Card's bytes down under a temporary name in the same directory as the Card, and
// returns that name. The temp name does not end in cardExt, so a scan never reads a half-written Card, and
// a reader never has to tell a partial file from a corrupt one.
func writeCardTemp(path string, raw []byte) (string, error) {
	// A fresh clone has none of the six columns in it, because git does not track empty directories, so the
	// directory a Card is published into is made here rather than by whoever is about to publish.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, writeTempPattern)
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// writeCardFile publishes a Card file over whatever is already at that path. It is the way an already
// located Card is rewritten - a move, an edit, a Comment - where the file there is the one that was read.
// Filing a new Card uses publishCardFile instead.
//
// The last look at that path is taken as late as this primitive can take it, after the temp file is complete
// and immediately before the rename. os.Rename's subject is the temp file, so nothing about it can notice what
// has happened to the Card's name: a hand, a git or another process that removed the Card mid-Write would be
// answered with the Card recreated under the bytes it had before it was removed, beside the copy that hand
// made, and no read would ever show the twin, because trash is not walked. So the rename is refused
// as ErrStale when the name is gone. The window this closes is a pair of syscalls wide, not shut: the shape
// that shuts it is travel's, where the act on the Card's own name is itself the check.
func writeCardFile(path string, raw []byte) error {
	name, err := writeCardTemp(path, raw)
	if err != nil {
		return err
	}
	defer os.Remove(name) // no-op once the rename below has succeeded
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: nothing is at that name to rewrite", ErrStale)
		}
		return err
	}
	return os.Rename(name, path)
}

// publishCardFile puts a Card file where none exists yet, and says so when one does. os.Link is the publish
// that cannot overwrite: os.Rename replaces the file at the destination, and a Card filed under the name
// this write had chosen would be gone with nothing saying it ever was.
func publishCardFile(path string, raw []byte) error {
	name, err := writeCardTemp(path, raw)
	if err != nil {
		return err
	}
	defer os.Remove(name) // no-op once the link below has succeeded
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrNameTaken, path)
		}
		return err
	}
	return nil
}

// travel moves a Card file from one name to another, and it is the only way a Card file changes directory.
// It links and unlinks rather than renaming, for the reason publishCardFile gives: os.Rename replaces whatever
// sits at the destination, and a Card already filed there under this name would vanish with nothing saying it
// ever was. A link that finds the name held is the twin fact cardFile refuses - one id, two files - and a link
// that finds nothing to link is a write that lost its race. Nothing is rewritten on the way, so the bytes a
// Card was committed with are the bytes that land at the new name, which is what lets git read a Move as a
// rename rather than as a deletion plus an addition.
//
// A Card file whose directory is on the other side of a Column - a Move, a reopen - and one whose directory is
// off the board entirely - a delete, a restore - travel the same way.
func travel(id, from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(to), err)
	}
	switch err := os.Link(from, to); {
	case err == nil:
	case errors.Is(err, os.ErrExist):
		// The name is held and the file this write was moving is still where it was, so one id is now held by
		// two Card files. The way out is in the file tree, not in a different request.
		return heldTwice(id, from, to)
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("card %s: %w - its file was not at %s when this write got there", id, ErrStale, from)
	default:
		// Named by the two directories, because those are where a hand has to look to make room.
		return fmt.Errorf("card %s: its file in %s could not be moved to %s: %w",
			id, filepath.Dir(from), filepath.Dir(to), err)
	}
	if err := os.Remove(from); err != nil {
		// The honest report of the half-done state: both files exist, and every write through this id will now
		// be refused until somebody removes one.
		return fmt.Errorf("card %s: its file is now in %s as well as in %s, and the one in %s would not remove: %w",
			id, to, from, from, err)
	}
	return nil
}

// Write is what one of the Board's Writes answers with: the Card as the board has it now, the Column it was
// in when the Write was asked for, and whether the Card's file changed at all. The model layer already promises the
// third fact - a Write that changes something stamps the Card's updated date, one that changes nothing moves
// nothing - and this is where that answer is said, so a door renders what happened instead of re-reading the
// file to guess it. The Card comes embedded, so a door that only needs the Card asks for the Write
// and reads its fields as it always did.
type Write struct {
	Card
	// From is the Column the Card sat in when this Write was asked for. A Move reads it as where the Card came
	// from; a Write that changed nothing reports the Column it is still in.
	From string
	// Changed says the file was written, and so stamped. A Write that changed nothing says so here rather than
	// in a sentence each door invents.
	Changed bool
}

// rewrite is the Board's one in-place Write: the bytes of a Card file it has just read, one decision applied
// to the JSON object those bytes are, and a publish that happens only when the decision changed something.
//
// The stamp belongs to this seam rather than to the decisions, and it keeps one promise: a Write that changes
// something stamps the Card's updated date, one that changes nothing moves nothing. Whether a decision changed
// anything is a
// fact of the Card's fields - they are encoded once before the decision and once after it, and equal encodings
// are the no-op a drag onto the Card's own gap, or the same Comment filed twice, reads as - so no door has to
// pre-compare its decision with the file to answer that, and a no-op neither writes nor stamps.
//
// A decision whose change is not a fact of this file is not this seam's question: a Card whose file has just
// travelled to another directory changed, while nothing in its fields says so. Those doors publish through
// stampCard.
//
// Whoever calls it keeps its own refusals: what the edit returns travels in the caller's words, and the bytes
// are still not decoded here - whoever read the file says when it is not a Card.
func rewrite(col, id, path string, raw []byte, when time.Time, edit func(fields map[string]any) error) (Write, error) {
	fields, err := decodeCard(raw)
	if err != nil {
		return Write{}, fmt.Errorf("card %s: %w", id, err)
	}
	encode := func() ([]byte, error) {
		written, err := cardBytes(fields)
		if err != nil {
			return nil, fmt.Errorf("card %s: its fields could not be encoded: %w", id, err)
		}
		return written, nil
	}
	unchanged, err := encode()
	if err != nil {
		return Write{}, err
	}
	if err := edit(fields); err != nil {
		return Write{}, err
	}
	changed, err := encode()
	if err != nil {
		return Write{}, err
	}
	if bytes.Equal(unchanged, changed) {
		// The decision landed on what the Card already said. Nothing is written, so the bytes this Card was read
		// with keep their bytes, and the Card that comes back is the one the board read.
		card, err := readCard(raw, col, id, when)
		if err != nil {
			return Write{}, err
		}
		return Write{Card: card, From: col}, nil
	}
	return stampCard(col, id, path, fields, when)
}

// stampCard publishes a Card the Board has decided on, stamped with the moment it changed. It is rewrite's
// other half: a Card whose file has just travelled changed because where it is changed - a Card's Column is its
// directory - and no field of the file records that, so there is nothing rewrite's compare could
// see. The Card's travel is finished before this begins: a publish laid on one name of a shared inode would
// leave the name the Card left behind holding the old bytes.
func stampCard(col, id, path string, fields map[string]any, when time.Time) (Write, error) {
	fields["updated"] = when.Format(time.RFC3339)
	written, err := cardBytes(fields)
	if err != nil {
		return Write{}, fmt.Errorf("card %s: its fields could not be encoded: %w", id, err)
	}
	if err := writeCardFile(path, written); err != nil {
		return Write{}, fmt.Errorf("card %s: its file in %s could not be written: %w", id, filepath.Dir(path), err)
	}
	card, err := readCard(written, col, id, when)
	if err != nil {
		return Write{}, err
	}
	// A door that publishes through stampCard has already moved the Card's file, so this Write changed
	// something whether or not its fields did.
	return Write{Card: card, From: col, Changed: true}, nil
}

// readDirs is every directory the Board reads a Card file out of: the Columns it walks, closed included, and
// trash, where a delete files a Card's file off the board. Ask a Board about one id and these are the places
// its filename can be.
var readDirs = append(append([]string{}, walkedColumns...), trashName)

// A held file is one directory's answer to "which file holds this id?": the directory that holds it, the
// file's full name, and its bytes. An id is a filename stem and a filename does not say which directory holds
// it, so the answer is a list and its length is the finding - one is the Card this id names, none is an id off
// the board, two is the twin nothing may write through.
type held struct {
	// dir is the directory's own name: a Column the Board walks, or trashName for a file it keeps off the board.
	dir string
	// path is that file in full, because the way out of a twin is to open one of them and rename it.
	path string
	// raw is its bytes. Whoever located them says when they are not a Card.
	raw []byte
}

// onBoard reports whether this file is a Card on the board rather than one off it. Only trash is off the walk:
// closed is walked, so a closed Card is a Card the board knows, lists and opens.
func (h held) onBoard() bool { return h.dir != trashName }

// filesHolding is the Board's one answer to "which file holds this id?", and the Board asks it two ways: a
// Card door looks an id up before it writes, and a walk meets the same fact when one id surfaces in two
// Columns. Both ask it of the same directories, and neither resolves a twin - naming it is the answer.
func (b *Board) filesHolding(id string) ([]held, error) {
	var found []held
	for _, dir := range readDirs {
		// cardPath names the file for any directory under .kanban, trash included.
		path := b.cardPath(dir, id)
		raw, err := os.ReadFile(path)
		if err == nil {
			found = append(found, held{dir, path, raw})
			continue
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s/%s: %w", dir, id, err)
		}
	}
	return found, nil
}

// pathsOf names the files a locate found, in full, the way a refusal has to name them.
func pathsOf(files []held) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.path)
	}
	return names
}

// cardFile is the lookup a Card door asks before it writes: the Column the Card is in, its path and its raw
// bytes. It is filesHolding read as a decision - none, one or two - so the twin refusal and the answers about
// where an id's file went are the ones every door already gives.
func (b *Board) cardFile(id string) (string, string, []byte, error) {
	found, err := b.filesHolding(id)
	if err != nil {
		return "", "", nil, err
	}
	var cards, off []held
	for _, h := range found {
		if h.onBoard() {
			cards = append(cards, h)
		} else {
			off = append(off, h)
		}
	}
	switch len(cards) {
	case 1:
		return cards[0].dir, cards[0].path, cards[0].raw, nil
	case 0:
		// An id that names no Card can still name one a delete took off the board, and "no Card with that id"
		// is a poor answer to a human who has just copied one. Naming it here is what lets read, comment and
		// move answer the same question with the same words.
		if len(off) > 0 {
			return "", "", nil, fmt.Errorf("%w %q: its file is in %s, which is where a delete puts it; back into a Column is zot-kanban restore, and that is the human's",
				ErrNoCard, id, b.trashDir())
		}
		return "", "", nil, fmt.Errorf("%w %q: a Card lives in one of %s", ErrNoCard, id, strings.Join(Columns, ", "))
	default:
		return "", "", nil, heldTwice(id, pathsOf(cards)...)
	}
}

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

// A commit name is a hexadecimal sha, in full or abbreviated to at least shaMinDigits. The shape is checked
// before the repository is asked, so a request that cannot be a commit never spawns a process, and a name can
// never reach git as an option rather than as a name.
const (
	hexDigits    = "0123456789abcdefABCDEF"
	shaMinDigits = 7
	shaMaxDigits = 64
)

func commitShaped(name string) bool {
	if len(name) < shaMinDigits || len(name) > shaMaxDigits {
		return false
	}
	return strings.Trim(name, hexDigits) == ""
}

// commitIsReal is the board's question to the repository behind it, and git is asked because git owns the
// object store. It answers for a commit and refuses everything else: another kind of object, a
// name the repository has never heard of, and a board sitting in a directory git does not know as a
// repository - a case the model layer rules out rather than assumes.
func (b *Board) commitIsReal(name string) error {
	if !commitShaped(name) {
		return badInput("%w: %q is not the shape of a commit name, which is %d to %d hexadecimal digits", ErrNoCommit, name, shaMinDigits, shaMaxDigits)
	}
	out, err := exec.Command("git", "-C", b.Root, "cat-file", "-t", name).CombinedOutput()
	answer := strings.TrimSpace(string(out))
	if err == nil && answer == "commit" {
		return nil
	}
	if answer == "" {
		answer = "nothing (git: " + strings.TrimSpace(err.Error()) + ")"
	}
	return conflict("%w: %q - git at %s says %s", ErrNoCommit, name, b.Root, answer)
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

// agentOrigin is the Card an Agent's door captured. The board says it without a word for it: the face
// wears its Priority stripe on the right edge rather than the left. A Card whose Origin was never
// recorded is nobody's to mark, so its stripe stays where a human's sits.
func agentOrigin(c Card) bool { return c.Origin == byAgent }

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
func (b *Board) Close(cardID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	col, _, _, err := b.cardFile(cardID)
	if err != nil {
		return err
	}
	if col != DoneColumn {
		return conflict("%s: card %s is in %s", ruleCloseFrom, cardID, col)
	}
	return b.offBoard(cardID, b.closedDir())
}

// CloseAllDone is the bulk door over finished work: every done Card whose done-ness holds up is closed, and a
// Card that contradicts its own commit stays in done, where the board prints the finding and the singular close
// is still the door a human can take it through. The bulk door checks because it moves a
// column a human is not looking at; the singular door does not, because there the human is looking at the Card
// the finding is printed on.
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
		if c.Contradiction != "" {
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

// trashedIDs names the ids .kanban/trash/ holds. They are not Cards, but their filenames still hold those
// ids: Add will not issue one, and Restore hands it back.
func (b *Board) trashedIDs() (map[string]bool, error) { return b.idsHeldIn(b.trashDir()) }

// closedIDs names the ids .kanban/closed/ holds. Those are Cards the board keeps rather than shows, and their
// ids are out of circulation the same way a deleted Card's are; a name held in both done and closed is the
// twin ErrAmbiguousID answers.
func (b *Board) closedIDs() (map[string]bool, error) { return b.idsHeldIn(b.closedDir()) }

// idsHeldIn names the ids one off-board directory holds by filename: its Cards are not among the Cards the
// board shows, but the filenames still hold those ids.
func (b *Board) idsHeldIn(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	held := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != cardExt {
			continue
		}
		held[strings.TrimSuffix(name, cardExt)] = true
	}
	return held, nil
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
	// is there is what comes back - the same reading the agent's door gives before it files, and
	// what makes a double click harmless. Comments are filed once and never edited; a Card is not a log.
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
	// reading the Card a second time. A correction names an ordinal and is answered by the rules
	// below, even when another Comment already carries these words.
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

// unseenPath and unseenCommentsPath are the board's read-state files: one Card id a line. Each holds only the
// ids still unseen, so it stays bounded by the Cards on the board rather than growing with the Cards ever
// opened, and each is gitignored so a mark is never a change in the repository.
func (b *Board) unseenPath() string         { return filepath.Join(b.Root, unseenFile) }
func (b *Board) unseenCommentsPath() string { return filepath.Join(b.Root, unseenCommentsFile) }

// readUnseen is one of those files as a set. No file is nothing unseen: a board nobody has filed onto yet, or a
// fresh clone, has read-state to record and has not recorded any.
func (b *Board) readUnseen(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	unseen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			unseen[id] = true
		}
	}
	return unseen, nil
}

// writeUnseen writes the whole set back, ids sorted, so the same set is the same bytes whichever order the
// Cards were filed in.
func (b *Board) writeUnseen(path string, unseen map[string]bool) error {
	ids := make([]string, 0, len(unseen))
	for id := range unseen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if err := os.WriteFile(path, []byte(strings.Join(ids, "\n")), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// markUnseen and forgetUnseen change one id in one read-state file, and their caller holds the lock the way
// every Board write does.
func (b *Board) markUnseen(path, cardID string) error {
	unseen, err := b.readUnseen(path)
	if err != nil {
		return err
	}
	if unseen[cardID] {
		return nil
	}
	unseen[cardID] = true
	return b.writeUnseen(path, unseen)
}

func (b *Board) forgetUnseen(path, cardID string) error {
	unseen, err := b.readUnseen(path)
	if err != nil {
		return err
	}
	if !unseen[cardID] {
		return nil
	}
	delete(unseen, cardID)
	return b.writeUnseen(path, unseen)
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

// counts reports the card count per column, in draw order, for cards already loaded. Keeping it pure means one
// Load answers a whole question instead of re-reading per column.
func counts(cards []Card) []int {
	counts := make([]int, len(walkedColumns))
	for _, c := range cards {
		for i, col := range walkedColumns {
			if c.Column == col {
				counts[i]++
				break
			}
		}
	}
	return counts
}

// holdersNamed says whether more than one Session is holding Cards on this board, which is when a Card's face
// has a reason to print which one holds it. A Claim kept by a Card in done, or carried into closed by the
// close of that same work, is not a hold: there it records who did the work, and the face names no holder at
// all. A Claim a hand wrote with no session
// named is held the same way the board already reads one: it names nobody, so it counts as nobody.
func holdersNamed(cards []Card) bool {
	holders := map[string]bool{}
	for _, c := range cards {
		if c.Claim == nil || c.Column == DoneColumn || c.Column == ClosedColumn || c.Claim.Session == "" {
			continue
		}
		holders[c.Claim.Session] = true
	}
	return len(holders) > 1
}
