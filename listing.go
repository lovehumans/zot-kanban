package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// listing.go is the read of the board off disk: the walk over the Columns it renders, the Column stamps that
// bracket a Card moving mid-walk, the Cards the walk sighted settles into what is listed, the order a Column
// displays, the counts a page and a summary read, and the check of the commit a done Card names. It takes no
// lock: the one mutex is held by the verb that asked.

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

// commitAnswer is what git said about one name the board asked about: the object the name resolved to, and what
// kind of object it is. The oid is kept because git resolves an abbreviation on the way to answering, and that
// full name is what a Card's seven digits were an abbreviation of.
type commitAnswer struct{ oid, kind string }

// commitTypes asks git, in one call, what each name is, and hands the answer to parseCommitAnswers to be read
// off. One call is what a board needs, because a board is re-read every few seconds and every Card in done is
// asked about each time. A board in a directory git does not know as a repository answers nothing, and that is
// returned as the error rather than read as "no commit anywhere".
func (b *Board) commitTypes(names []string) (map[string]commitAnswer, error) {
	var stderr strings.Builder
	cmd := exec.Command("git", "-C", b.Root, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	answers := parseCommitAnswers(names, string(out))
	if err != nil && len(answers) == 0 {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		return nil, fmt.Errorf("git at %s says %s", b.Root, reason)
	}
	return answers, nil
}

// parseCommitAnswers reads git's answer off its lines: "<oid> <type> <size>" for a name the repository has, and
// "<name> missing" for one it does not, in the order the names were given. A line is taken as an answer for a
// name only when the object it names is one git could have produced for that name - the oid is the name asked,
// or an extension of it - because git spends more than one line on an abbreviation it resolves to more than one
// object, and every name after that one would otherwise be read against the wrong line. A name left without an
// answer is refused as one the repository has no commit of: the board would rather call checked work unchecked
// than carry a name it is not sure the repository gave.
func parseCommitAnswers(names []string, out string) map[string]commitAnswer {
	answers := map[string]commitAnswer{}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, name := range names {
		if i >= len(lines) {
			continue
		}
		fields := strings.Fields(lines[i])
		if len(fields) < 2 {
			continue
		}
		oid := fields[0]
		if len(oid) < len(name) || !strings.EqualFold(oid[:len(name)], name) {
			continue
		}
		answers[name] = commitAnswer{oid: oid, kind: fields[1]}
	}
	return answers
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
	answers, err := b.commitTypes(names)
	for i, c := range cards {
		if (c.Column != DoneColumn && c.Column != ClosedColumn) || !asking[c.Commit] {
			continue
		}
		if answer := answers[c.Commit]; answer.kind == "commit" {
			// The repository answered, and answered with the full oid: the name a Card filed with an
			// abbreviation was an abbreviation of.
			cards[i].CommitOID = answer.oid
			continue
		}
		if err != nil {
			cards[i].Contradiction = fmt.Sprintf("names %s, and %s", c.Commit, err)
			continue
		}
		cards[i].Contradiction = fmt.Sprintf(noSuchCommitFmt, c.Commit)
	}
}

// commitCopy is the commit name the Card view hands the clipboard, and the name its short print is a print of:
// the oid git answered for a done Card's commit, or the name the Card's file holds when the repository answered
// nothing to resolve it with. A Card filed with seven digits and one filed with forty are therefore copied the
// same way, because the name a repository knows a commit by is its full oid.
func commitCopy(c Card) string {
	if c.CommitOID != "" {
		return c.CommitOID
	}
	return c.Commit
}

// markContradiction asks a Card on its own the question the board asks of all of them, so a Card shown in the
// modal is marked the way the same Card is marked on the board.
func (b *Board) markContradiction(c *Card) {
	one := []Card{*c}
	b.markDoneContradictions(one)
	*c = one[0]
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
