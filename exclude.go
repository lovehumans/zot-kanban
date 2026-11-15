package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The board's working files under .kanban: the address its server bound and the two stores the new marks
// are read off. None is a Card and none is a record a hand would commit, so the board
// arranges for git to ignore all three rather than leaving a hand to notice them in `git status` and write
// the rules by hand.
//
// trash is deliberately not among them. Whether a deleted Card is tracked is a human decision, and the
// untracked `?? .kanban/trash/` a delete leaves is what makes that decision visible at all.
var volatileFiles = []string{urlFile, unseenFile, unseenCommentsFile}

// excludeHeader introduces the board's lines in a file the board shares with the human's own rules, so a
// reader of it can tell which lines a tool wrote and which they wrote themselves.
const excludeHeader = "# zot-kanban: the board's own working files"

const (
	infoDirName  = "info"
	excludeName  = "exclude"
	excludeMode  = 0o644 // the exclude file a human may open and edit is an ordinary file, not a private one
	checkNoMatch = 1     // git's answer "no rule covers any of these yet", which is an answer and not a failure

	// gitProgram is the one external command this file runs, and only ever to ask a question. A board never
	// tells git to change a repository.
	gitProgram = "git"
)

// excludeResult is what one pass over the working files did. An empty note is the steady state: everything
// already arranged, said by nobody.
type excludeResult struct {
	// excludePath is where the lines would go, relative to the project when git lets it be relative.
	excludePath string
	// added are the names written, each anchored where the repository begins: .kanban/url in a project
	// whose repository is the project, and url in one whose repository is the board directory itself.
	added []string
	// tracked are the names git already holds in its index. Excluding a tracked file changes nothing, so
	// the board leaves it tracked and tells the human that is why nothing was written for it.
	tracked []string
	// failure is git's own words when the board could not ask git or could not write, kept for the line
	// that announces it. A board that cannot arrange its exclusions still serves.
	failure string
}

// excludeVolatile arranges git's answer for the board's working files in info/exclude, the exclude file git
// consults that no clone carries and no diff shows. It runs once per Board, at the point a board starts
// writing those files, because a board is re-read every few seconds and git is a subprocess each time.
//
// Two questions go before any byte is written, so the board adds its rules to a human's rather than over
// them. First, whether a rule already covers the name - a `.gitignore` line a hand wrote is not repeated in
// a file nobody looks at. Second, whether git already tracks the name, which no exclude line can undo.
//
// A directory git does not know as a repository is not a failure: a board works there all the same and has
// nothing to arrange and nothing to say. Everything else a git question can answer is kept for the notice.
func (b *Board) excludeVolatile() excludeResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.excludeRan {
		return b.excludeRun
	}
	res := b.excludeVolatileOnce()
	b.excludeRan = true
	b.excludeRun = res
	return res
}

// excludeVolatileOnce is the one pass excludeVolatile keeps the answer to, with the Board's lock already held.
func (b *Board) excludeVolatileOnce() excludeResult {
	res := excludeResult{excludePath: filepath.Join(b.Root, infoDirName, excludeName)}

	gitDir, top, err := b.gitPlaces()
	if err != nil {
		return res // no repository here: nothing to arrange, and the board says nothing about it.
	}
	res.excludePath = b.nearProject(filepath.Join(gitDir, infoDirName, excludeName))

	// The questions are asked from the board directory, where git reads a bare name as this board's file. What
	// gets written is the name as the repository reads it, anchored at its own root, so a line never ignores a
	// file named url in some other part of the project.
	prefix := boardPrefix(b.Root, top)
	written := make([]string, 0, len(volatileFiles))
	for _, name := range volatileFiles {
		written = append(written, filepath.Join(prefix, name))
	}

	covered, err := b.gitCovers(volatileFiles)
	if err != nil {
		res.failure = err.Error()
		return res
	}
	tracked, err := b.gitTracks(volatileFiles)
	if err != nil {
		res.failure = err.Error()
		return res
	}
	for i, name := range volatileFiles {
		if tracked[name] {
			res.tracked = append(res.tracked, written[i])
			continue
		}
		if !covered[name] {
			res.added = append(res.added, written[i])
		}
	}
	if len(res.added) == 0 {
		return res
	}
	if err := appendExcludeLines(filepath.Join(gitDir, infoDirName, excludeName), res.added); err != nil {
		res.failure = err.Error()
		res.added = nil
	}
	return res
}

// gitPlaces asks git where its own directory is and where the repository it belongs to begins. The board
// asks from its own directory, the way it already asks git about a commit name, so a repository one level
// up is found the same way. A worktree or submodule whose .git is a file is answered by git,
// which is why the board never builds this path itself.
func (b *Board) gitPlaces() (gitDir, top string, err error) {
	out, err := exec.Command(gitProgram, "-C", b.Root, "rev-parse", "--absolute-git-dir", "--show-toplevel").Output()
	if err != nil {
		return "", "", fmt.Errorf("git does not know %s as a repository", b.Root)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return "", "", fmt.Errorf("git answered %q for where its repository is", strings.TrimSpace(string(out)))
	}
	return lines[0], lines[1], nil
}

// boardPrefix is the path from the repository's root to the board's directory, which is what makes an
// exclude line say this board's file. A repository rooted at the project gets .kanban/url; one rooted at
// the board directory itself gets url, where a .kanban/ prefix would name a directory that is not there.
func boardPrefix(boardRoot, top string) string {
	rel, err := filepath.Rel(top, boardRoot)
	if err != nil || rel == "." {
		return ""
	}
	return rel
}

// gitCovers is which of these names a rule already covers, read off one git call. Names are given as the
// board reads them, from its own directory, because git reads a bare name against that directory's prefix -
// which is the same reading it applies to a git mv of a Card. --no-index keeps the question about rules
// rather than about the index, so a tracked name still gets its answer; git prints each name a rule reaches,
// and exits 1 when it reaches none - which is the answer "not yet", kept apart from a git that could not
// answer at all.
func (b *Board) gitCovers(names []string) (map[string]bool, error) {
	cmd := exec.Command(gitProgram, "-C", b.Root, "check-ignore", "--no-index", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(names, "\n") + "\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == checkNoMatch {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("git check-ignore: %s", gitSaid(stderr.String(), err))
	}
	covered := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			covered[line] = true
		}
	}
	return covered, nil
}

// gitTracks is which of these names git already holds in its index. No exclude line un-tracks a file, so the
// board writes none for a tracked name and says the file is tracked instead.
func (b *Board) gitTracks(names []string) (map[string]bool, error) {
	full := append([]string{"-C", b.Root, "ls-files", "--"}, names...)
	var stderr strings.Builder
	cmd := exec.Command(gitProgram, full...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %s", gitSaid(stderr.String(), err))
	}
	tracked := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tracked[line] = true
		}
	}
	return tracked, nil
}

// gitSaid is what git told us in its own words, which is a human's clue, with the exit's words behind it
// when git said nothing. A board already asks git this way about a commit name.
func gitSaid(stderr string, err error) string {
	if said := strings.TrimSpace(stderr); said != "" {
		return said
	}
	return err.Error()
}

// appendExcludeLines publishes info/exclude with the board's lines after what it already holds, by temp file
// and rename: the file a human's next git command reads is never the half of itself. An absent exclude file
// is an empty one, and an absent info directory is one the board makes.
func appendExcludeLines(path string, names []string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	body := string(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	lines := []string{}
	if !strings.Contains(body, excludeHeader) {
		lines = append(lines, excludeHeader)
	}
	for _, name := range names {
		if !excludeHolds(body, name) {
			lines = append(lines, name)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += strings.Join(lines, "\n") + "\n"

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, writeTempPattern)
	if err != nil {
		return fmt.Errorf("create the exclude file to rename: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // only the rename keeps it; anything short of that leaves nothing behind
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Chmod(excludeMode); err != nil {
		tmp.Close()
		return fmt.Errorf("set %s readable: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}

// excludeHolds reports whether the file already carries this name as one of its own lines, so a second pass
// writes nothing a first one arranged.
func excludeHolds(body, name string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

// nearProject is a path the human can read back: the exclude file from the project they are standing in,
// or git's own path when it sits on another filesystem and no relative name reaches it.
func (b *Board) nearProject(path string) string {
	root := filepath.Dir(b.Root)
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return path
}

// announce is what a board says about one pass, on the line it already prints: what it wrote, or the one
// thing a human might act on. Silence is the steady state - rules already arranged are no news.
func (r excludeResult) announce() string {
	switch {
	case r.failure != "":
		return " — could not arrange its exclusions in " + r.excludePath + ": " + r.failure
	case len(r.added) > 0:
		note := " — excluded " + listPhrase(r.added) + " in " + r.excludePath
		if len(r.tracked) > 0 {
			note += "; git already tracks " + listPhrase(r.tracked) + ", so those stay tracked"
		}
		return note
	case len(r.tracked) > 0:
		return " — git already tracks " + listPhrase(r.tracked) + ", so nothing was excluded"
	default:
		return ""
	}
}

// listPhrase is a board's "a, b and c" for a handful of names: a comma between the names before the last, and
// "and" between the last two, which is how a board names a few files rather than printing what a table would
// want. Two names need no comma at all.
func listPhrase(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
