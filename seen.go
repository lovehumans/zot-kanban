package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
