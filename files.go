package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// files.go is the file seam: where a Card's file is for each directory the Board knows, the publish primitive
// per direction a Card file travels by, the locate that answers which file holds an id, and the in-place rewrite
// a patch lands through. A Card file is published whole, so a lone read of one needs no lock.

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
