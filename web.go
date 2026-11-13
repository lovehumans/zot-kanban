package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The board is a presentation of the filesystem, not a copy of it. Every read goes back to
// .kanban/, so a card an agent edited with its file tools is on the board immediately.
//
// There is no push path: zot exposes no way to wake a running extension, so the browser
// asks for the board again. A refresh returns a column rather than the whole board, because moving
// a card means changing which directory holds it - one column is what a move has to re-fetch.
//
// view.go holds the data this file renders: the view a request asked the page to show, the Cards that view
// reaches, and the columns and counts the page prints. web.go is the doors - the routes, their refusals, and
// the render that turns one template into a response.

//go:embed web
var webFiles embed.FS

// defaultPort is 7498: "kanban" in leet (k=7 a=4 9 n b=8). It sits below this machine's ephemeral
// range (32768-60999), so the OS has not handed it to something else at random.
const defaultPort = 7498

// boardStamp is the shape this board has always printed a date in, and it is what a Card shows once the
// relative words run out: a time a week old is a date, not a countdown.
const boardStamp = "2006-01-02 15:04"

const (
	daySpan  = 24 * time.Hour
	weekSpan = 7 * 24 * time.Hour
)

// relativeWord is how long ago a time is, in words a human reads without arithmetic. A time ahead of the
// server's clock - a hand-edited file, two machines disagreeing - reads "just now", because a Card from
// the future is not a fact the board has a word for.
func relativeWord(at, now time.Time) string {
	elapsed := now.Sub(at)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < daySpan:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 2*daySpan:
		return "yesterday"
	case elapsed < weekSpan:
		return fmt.Sprintf("%d days ago", int(elapsed.Hours()/24))
	default:
		return at.Format(boardStamp)
	}
}

// commentFoldLines is how many lines of a Comment the modal shows before the rest waits behind a click. Ten
// is about the length of a Comment a human writes in one go: the point of it fits without opening anything,
// and a Card carrying several still fits the modal. The sheet clamps to the same number (web/gruvbox.css), so
// the two are written beside each other on purpose.
const commentFoldLines = 10

// commentFoldWidth is roughly how many characters one line of a Comment holds in the modal: the sheet sets the
// page in 15px monospace (body in web/gruvbox.css, about 9px per glyph) and the modal in min(92vw, 42rem)
// less its own padding and the Comments block's.
const commentFoldWidth = 70

// foldable says whether a Comment runs past the lines the modal shows, counting the lines a browser will wrap
// a long one into rather than the newlines the text carries: a Comment of one long line wraps too, which is
// what counting source lines got wrong. Only the decision of whether a Comment is worth making clickable lives
// here; how much of it the page shows is measured on the page.
//
// ponytail: commentFoldWidth is an estimate, exact only at the modal's full width - a narrow window wraps more
// than this counts, so a borderline Comment shows more than commentFoldLines there. The upgrade is a
// measurement in web/board.js (scrollHeight against clientHeight) if that ever matters.
func foldable(text string) bool {
	lines := 0
	for _, line := range strings.Split(text, "\n") {
		lines += (len([]rune(line)) + commentFoldWidth - 1) / commentFoldWidth
	}
	return lines > commentFoldLines
}

type Server struct {
	board   *Board
	cwd     string
	install string
	url     string
	ln      net.Listener
	mux     *http.ServeMux
	tmpl    *template.Template
}

func NewServer(root, installDir string) (*Server, error) {
	// since is the one formatting the board does for a time, and it renders the whole element: relative
	// words for anything under a week old - "just now", "5m ago", "3h ago", "yesterday", "3 days ago" -
	// and the timestamp itself from a week old on, because "12 days ago" asks a human to count back from a
	// date they were not shown. The full stamp travels in datetime and in title, so the exact moment is a
	// hover away whatever the words say. A Comment a hand wrote with no time renders nothing at all: a bare
	// time.Time is "0001-01-01 00:00:00 +0000", which the board must not show.
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"since": func(t time.Time) template.HTML {
			if t.IsZero() {
				return ""
			}
			stamp := t.Format(time.RFC3339)
			return template.HTML(fmt.Sprintf("<time datetime=%q title=%q>%s</time>",
				stamp, stamp, relativeWord(t, time.Now())))
		},
		// owner is the Comment's Owner in the board's own words. The board is read by the
		// human, so a human's Comment is "you". A Comment with no by was typed into the file: only the
		// tool writes, and the tool always names the agent. The session id stays out of the board; it is
		// what the tool checks when it decides which Comment an agent may rewrite, not board information.
		"owner": func(c Comment) string {
			switch c.By {
			case byAgent:
				return "agent"
			case byHuman, "":
				return "you"
			default:
				return c.By
			}
		},
		// priorities is the Card's own vocabulary, taken from the same constants Board.Edit validates
		// against, so the edit form's choices and the model layer's refusals cannot drift apart.
		"priorities": func() []string { return []string{priorityHi, priorityMi, priorityLo} },
		// defaultPriority is the priority a Card gets when its writer names none, the one normalize
		// gives, so the composer's pre-selected choice is the model's own default rather than a string
		// a template invented.
		"defaultPriority": func() string { return priorityMi },
		// nextCommentID is the ordinal the next Comment on this Card will take, the same number the model
		// layer assigns. The form carries it, so a Comment that arrives twice is recognised as the one it
		// already is instead of being filed a second time.
		"nextCommentID": nextCommentID,
		// foldable is the Comments region's own measure: which Comment runs past the lines the modal shows, and
		// is therefore the one a hand clicks open. How much of it is shown is the sheet's, not this call's.
		"foldable": foldable,
		// commentCountTitle is the words a Comment count keeps when the face prints a number beside a bubble instead
		// of a word: the title a hover reads, and a reading of the Card board.go builds rather than a sentence a
		// template works out ( as 0.70.0).
		"commentCountTitle": commentCountTitle,
		// movable and ruleFor are the Column gate's answers, quoted rather than restated: the Board
		// says which Card a hand may lift out of its Column - the fact card.fragment.html asks three times about
		// one Card, for the drag, the Claim's edge and the holder's name - and what a Column nothing lands in
		// prints under its own name. The page still needs both before any request is made, because a Column that
		// never accepts a drop never gets one to refuse: the reason has to be on the page for the drag to read it
		// back (web/board.js). The words are words, not a citation.
		"movable": movable,
		"ruleFor": ruleFor,
		// agentOrigin is the face's access to a Card's Origin, read from the same constant the two filing doors write.
		// The sheet takes it as which side of the Card the Priority stripe wears, so the mark
		// comes from the field and never from words a Card file happens to carry.
		"agentOrigin": agentOrigin,
		// stepCount, orderedSteps, unmetSteps, steppable, stepsLocked, ladderOf and stepsFull are the page's access to
		// a Card's steps: the x/n a face prints, the order a modal lists them in, the steps a finished Card still shows
		// undone, which of them a hand may put a live control on with the sentence that says why not, the ladder a Card's
		// own view draws with its spent rungs in it, and whether the Card is already full of live steps. All seven are
		// readings of the Card rather than fields of its file, so the page quotes board.go for them the way it quotes
		// movable and agentOrigin instead of working them out in a template.
		"stepCount":    stepCount,
		"orderedSteps": orderedSteps,
		"unmetSteps":   unmetSteps,
		"steppable":    steppable,
		"stepsLocked":  stepsLocked,
		"ladderOf":     ladderOf,
		"stepsFull":    stepsFull,
		// stepNameLimit and stepFieldHint are the filing field's own limit and its tooltip, both read from the numbers
		// the step door counts: the field refuses a name longer than the door would file, and its tooltip says which of
		// the two rooms a full Card is out of. The door counts runes; the field counts what a browser counts.
		"stepNameLimit": func() int { return stepNameLimit },
		"stepFieldHint": stepFieldHint,
		// agentRules is the help modal's way of quoting the board's rules instead of restating them:
		// they have one published home in the tool description, and a page that printed its own wording of them
		// would be a second copy that can drift from what an Agent was handed. The words are the same const, verbatim.
		"agentRules": func() string { return boardRules },
		// tilde is a directory written the way a shell writes it. The header names the project the human is
		// already standing in, and its home prefix is the part nobody reads; os.UserHomeDir is asked each
		// render rather than assumed, because the home belongs to whoever is looking.
		"tilde": func(path string) string {
			home, err := os.UserHomeDir()
			if err != nil || !underPath(path, home) {
				return path
			}
			return "~" + strings.TrimPrefix(path, home)
		},
	}).ParseFS(webFiles, "web/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse board templates: %w", err)
	}
	s := &Server{board: NewBoard(root), cwd: root, install: installKind(root, installDir), tmpl: tmpl}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/api/board", s.handleBoard)
	s.mux.HandleFunc("/api/column", s.handleColumn)
	s.mux.HandleFunc("POST /api/card", s.handleCardNew)
	s.mux.HandleFunc("PATCH /api/card/{id}", s.handleCardPatch)
	s.mux.HandleFunc("GET /api/card/{id}", s.handleCardDetail)
	s.mux.HandleFunc("POST /api/card/{id}/comment", s.handleCardComment)
	s.mux.HandleFunc("POST /api/card/{id}/seen", s.handleCardSeen)
	s.mux.HandleFunc("GET /api/card/{id}/comments", s.handleCardComments)
	// A step is written through its own route rather than as a field on the edit form: it is one ordinal and the
	// action its box's own state names, and a control that ticks one step does not travel with the title, body and
	// priority a save submits.
	s.mux.HandleFunc("POST /api/card/{id}/step", s.handleCardStep)
	s.mux.HandleFunc("DELETE /api/card/{id}", s.handleCardDelete)
	// Closing and reopening are their own routes rather than a field on the edit form or a shape of a move:
	// they are the two Card actions a browser may take that no Agent action and no drag is, and a door only a
	// human hand opens is easier to see when it is not one more key in a body.
	s.mux.HandleFunc("POST /api/card/{id}/close", s.handleCardClose)
	s.mux.HandleFunc("POST /api/card/{id}/reopen", s.handleCardReopen)
	s.mux.HandleFunc("/static/", s.handleStatic)
	return s, nil
}

// nextFreePort walks up to 10 ports from want and returns the one it bound, on 127.0.0.1 only. A
// second zot session in another project holds the first port, and a restarting session races for
// it, so the port actually bound is reported rather than assumed.
func nextFreePort(want int) (net.Listener, int, error) {
	for port := want; port < want+10; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, port, nil
		}
		if !strings.Contains(err.Error(), "address already in use") {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("no free port from %d to %d", want, want+9)
}

// Listen binds the port and records the address in the board directory. It runs during the
// handshake, not in a goroutine, so the address the extension reports is the one bound.
func (s *Server) Listen(port int) error {
	ln, bound, err := nextFreePort(port)
	if err != nil {
		return err
	}
	return s.bound(ln, bound)
}

// ListenExactly binds the port it is given and no other. Walking upward is the convenience of a
// board nobody named a port for; once a human has named one, walking is the lie the bound address
// would tell them, so a taken port is refused by name instead of being walked past.
func (s *Server) ListenExactly(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			return fmt.Errorf("port %d is taken: %w", port, err)
		}
		return fmt.Errorf("bind port %d: %w", port, err)
	}
	return s.bound(ln, port)
}

// bound records what was actually bound, so url answers with the address rather than the wish.
func (s *Server) bound(ln net.Listener, port int) error {
	s.ln = ln
	s.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := s.board.WriteURL(s.url); err != nil {
		return fmt.Errorf("record board url: %w", err)
	}
	return nil
}

// Serve answers requests until the listener is closed. Nothing closes it: zot kills the extension
// and the OS closes the socket.
func (s *Server) Serve() error {
	if s.ln == nil {
		return errors.New("board server is not listening")
	}
	return http.Serve(s.ln, s.mux)
}

// boardURL is the address to tell the human, which only exists once Listen has succeeded.
func (s *Server) boardURL() string { return s.url }

// handleRoot renders the whole document, shell and board in one pass, so opening the address a
// session reported costs one request and always arrives as a styled page. Only "/" is the board:
// any other path is 404 rather than another copy of it.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cards, err := s.board.Load()
	s.write(w, "board.html", s.data(cards, err, viewOf(r)))
}

// handleBoard renders just the board fragment: what the browser polls every few seconds. It has no
// shell and no stylesheet link, so it is never the page a human opens.
func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	cards, err := s.board.Load()
	s.write(w, "index.html", s.data(cards, err, viewOf(r)))
}

// handleColumn renders one column, which is what a card move re-fetches. A request for a column
// outside the six answers 400 and names the legal ones: a wrong column in a move means a card
// landing in a directory the board never renders. closed is the one Column a page can be asked to draw that
// this route never answers: a fragment request is what a Card move re-fetches, and closed is no move's
// destination - a Card leaves it by reopening.
func (s *Server) handleColumn(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("col")
	if !validColumn(name) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "<p class=\"error\">unknown column %q; the columns are %s</p>", name, strings.Join(Columns, ", "))
		return
	}
	cards, err := s.board.Load()
	if err != nil {
		s.write(w, "index.html", s.data(cards, err, viewOf(r)))
		return
	}
	s.write(w, "column.fragment.html", s.column(name, filterCards(cards, viewOf(r).Query)))
}

// handleCardNew is the door a browser uses to create a Card, and it files into inbox and no other Column:
// what a Card does after inbox is triage, which is the human's, and a request free to name any Column would
// be free to name done. It is the human's door, so the Card it files carries `human` as its Origin
// and arrives without the board's new mark, which is an Agent Card's to carry. The fields it may
// name are the ones a Card is born with - title, body, priority. A request that names a Column or an order is
// refused rather than quietly narrowed to what this handler would ignore: order is the model layer's, which
// puts a new Card at the end of its Column, and a Column is what a move decides afterwards. The answer is the
// one every accepted write gives, the Column the Card landed in in an <hx-partial> and kanban:saved on the way
// back.
func (s *Server) handleCardNew(w http.ResponseWriter, r *http.Request) {
	fields, err := requestedFields(r)
	if err != nil {
		reject(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case fields.Column != "" && fields.Column != FirstColumn:
		reject(w, http.StatusBadRequest, fmt.Errorf("a new card is filed into %s, not %q: file it, then move it with a PATCH", FirstColumn, fields.Column))
		return
	case fields.Order != nil:
		reject(w, http.StatusBadRequest, fmt.Errorf("a new card is filed at the end of %s: order is what a PATCH changes", FirstColumn))
		return
	case fields.Above != "":
		reject(w, http.StatusBadRequest, fmt.Errorf("a new card is filed at the end of %s: above is what a drag chooses on a PATCH", FirstColumn))
		return
	}
	title, body, priority := fields.filedWith()
	card, err := s.board.Add(FirstColumn, title, body, priority, time.Now())
	if err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	s.writeColumns(w, r, []string{card.Column})
}

// handleCardPatch is the door a browser uses to change a Card it already has. A request names a
// Column to carry the Card to, or fields to change on the Card where it already is, or both. A request that
// names a place - above which Card the Card lands, or PlaceEnd for after the Column's last Card - is a drag:
// it sorts the Card into that place in the Column it lands in, and it may name no Column at all, which is the
// Column the Card already sits in. Naming a place and an Order number in one request is refused, because they
// are the same change named twice. A move into done is refused by the Board's Column gate, the same answer the
// tool gets, because no browser request carries a commit sha; the text of a Card that is already in done is
// editable, since editing text moves nothing, and no door carries a Card out of done at all. A human
// filing a Card by hand renames the file and records the commit.
// The response names each Column the request changed
// in an <hx-partial>, htmx 4's way of saying "this response updates these regions", so one
// request updates the page instead of leaving the Card visible where it was.
func (s *Server) handleCardPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fields, err := requestedFields(r)
	if err != nil {
		reject(w, http.StatusBadRequest, err)
		return
	}
	from, _, _, err := s.board.cardFile(id)
	if err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	if fields.Above != "" && fields.Order != nil {
		reject(w, http.StatusBadRequest, fmt.Errorf("card %s: a place to land and an order number are the same change named twice - send above, or order, not both", id))
		return
	}
	if fields.Column == "" && fields.Above != "" {
		// A drag that sorts a Card within the Column it sits in names no Column, because there is nothing to
		// carry it to.
		fields.Column = from
	}
	if fields.Column == "" && !fields.hasFields() {
		reject(w, http.StatusBadRequest, fmt.Errorf(`card %s: a PATCH wants "column" or one of title, body, priority, order`, id))
		return
	}

	var columns []string
	if fields.Column != "" {
		moved, err := s.board.Move(id, fields.Column, "", time.Now(), HumanMove.at(fields.Above))
		if err != nil {
			// A Card another writer moved first, or a Card another session holds, is a conflict rather than
			// bad input: nothing this request could send makes it right, while the next poll may.
			reject(w, refusalStatus(err), err)
			return
		}
		columns = append(columns, from, moved.Column)
	}
	if fields.hasFields() {
		edited, err := s.board.Edit(id, fields.edit(), time.Now())
		if err != nil {
			reject(w, refusalStatus(err), err)
			return
		}

		columns = append(columns, edited.Column)
	}
	s.writeColumns(w, r, columns)
}

// cardFields is what one Card write may name, through either door: a Column to move to, fields to change on
// the Card where it stands, or both. A field that is nil is a field the request did not name, so a form that
// posts a title on its own cannot blank a body.
type cardFields struct {
	Column   string
	Title    *string
	Body     *string
	Priority *string
	Order    *int
	// Above is the place a drag named on a write that moves or sorts: the id of the Card the Card lands
	// above, or PlaceEnd for after the Column's last Card. Empty names no place.
	Above string
}

func (p cardFields) hasFields() bool {
	return p.Title != nil || p.Body != nil || p.Priority != nil || p.Order != nil
}

func (p cardFields) edit() CardEdit {
	return CardEdit{Title: p.Title, Body: p.Body, Priority: p.Priority, Order: p.Order}
}

// filedWith are the three fields a Card is created with, "" where the request named none: Board.Add gives an
// empty priority its own default and refuses an empty title, so neither rule is restated here.
func (p cardFields) filedWith() (string, string, string) {
	named := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	return named(p.Title), named(p.Body), named(p.Priority)
}

// requestedFields reads either body shape a Card write arrives in: htmx sends a form, curl sends JSON, and
// a form says "the field is absent" by not carrying the key at all.
func requestedFields(r *http.Request) (cardFields, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Column   *string `json:"column"`
			Title    *string `json:"title"`
			Body     *string `json:"body"`
			Priority *string `json:"priority"`
			Order    *int    `json:"order"`
			Above    *string `json:"above"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return cardFields{}, fmt.Errorf(`body is not a JSON object with "column", "title", "body", "priority", "order" or "above": %w`, err)
		}
		out := cardFields{Title: in.Title, Body: in.Body, Priority: in.Priority, Order: in.Order}
		if in.Column != nil {
			out.Column = strings.TrimSpace(*in.Column)
		}
		if in.Above != nil {
			out.Above = strings.TrimSpace(*in.Above)
		}
		return out, nil
	}
	if err := r.ParseForm(); err != nil {
		return cardFields{}, fmt.Errorf("body is not a form or a JSON object: %w", err)
	}
	out := cardFields{Column: strings.TrimSpace(r.PostForm.Get("column"))}
	if v := r.PostForm.Get("title"); r.PostForm.Has("title") {
		out.Title = &v
	}
	if v := r.PostForm.Get("body"); r.PostForm.Has("body") {
		out.Body = &v
	}
	if v := r.PostForm.Get("priority"); r.PostForm.Has("priority") {
		out.Priority = &v
	}
	out.Above = strings.TrimSpace(r.PostForm.Get("above"))
	if raw := r.PostForm.Get("order"); r.PostForm.Has("order") {
		order, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return cardFields{}, fmt.Errorf("order %q: want a whole number", raw)
		}
		out.Order = &order
	}
	return out, nil
}

// writeColumns answers with one <hx-partial> per Column the request changed, and HX-Trigger: kanban:saved
// so the page knows a Card write landed: web/board.js closes the modal a save was made from on that event.
// A Card edited in place changes one Column; a Card moved changes the two.
func (s *Server) writeColumns(w http.ResponseWriter, r *http.Request, columns []string) {
	cards, err := s.board.Load()
	if err != nil {
		reject(w, http.StatusInternalServerError, err)
		return
	}
	// A write answers the Columns it changed, and it answers them in the view the request asked for.
	// Otherwise the common case - filter to find a Card, then drag it - would have the whole board flash
	// back for the few seconds until the next poll. The closed Column is answered only on a page drawing it:
	// htmx replaces the regions a response names, and a name no element on the page answers to would swap
	// nothing while claiming it had.
	view := viewOf(r)
	cards = filterCards(cards, view.Query)
	var buf bytes.Buffer
	seen := map[string]bool{}
	for _, name := range columns {
		if name == "" || seen[name] || (name == ClosedColumn && !view.Closed) {
			continue
		}
		seen[name] = true
		fmt.Fprintf(&buf, "<hx-partial hx-target=\"#col-%s\" hx-swap=\"outerHTML\">\n", name)
		if err := s.tmpl.ExecuteTemplate(&buf, "column.fragment.html", s.column(name, cards)); err != nil {
			reject(w, http.StatusInternalServerError, fmt.Errorf("board render failed: %w", err))
			return
		}
		buf.WriteString("</hx-partial>\n")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("HX-Trigger", "kanban:saved")
	_, _ = w.Write(buf.Bytes())
}

// handleCardDetail renders one Card in full, edit form included: the body, the Comments, the dates, the
// priority, the order and the commit that filed it. The board shows none of that; this is what
// the modal swaps in, and it is also the whole Card for curl or for a human reading one Card at a time. A
// response of one Card fragment carries no shell and no stylesheet, so it is never a page.
func (s *Server) handleCardDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	card, err := s.readCardFor(id)
	if err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	// A Card shown whole is asked the board's done question too, so the modal cannot show a Card filed done
	// that the board itself would mark.
	s.board.markContradiction(&card)
	s.write(w, "card.detail.html", card)
}

// handleCardSeen records that somebody has opened this Card, which is the one thing that clears the marks the
// board puts on it: the new mark on a Card an Agent filed and the mark on a Comment an Agent filed.
// web/board.js fires it on the swap that puts a Card into the modal, so opening the Card is the act
// that marks it read rather than a control beside it. It answers empty - the Card is already on the screen - and
// it deliberately carries no kanban:saved, because that event closes the modal and this request arrives while the
// Card is open. The marks behind the modal are the next poll's to redraw, in every browser looking at this board:
// the read-state is the board's, not one viewer's.
func (s *Server) handleCardSeen(w http.ResponseWriter, r *http.Request) {
	if err := s.board.Seen(r.PathValue("id")); err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCardComment files one Comment on a Card through the door a human uses: the modal's own form, where
// the author is the human and the timestamp is the server's. It answers the Card's Comments region - the
// filed Comments and an empty form carrying the next ordinal - because the region the human posted from is
// the thing that changed, and swapping it puts their Comment where they typed it. It announces
// kanban:commented, not kanban:saved: a Comment does not finish with the Card, and the modal that filed one
// stays open.
func (s *Server) handleCardComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	text, wants, err := requestedComment(r)
	if err != nil {
		reject(w, http.StatusBadRequest, err)
		return
	}
	filed, _, err := s.board.FileComment(id, text, wants, time.Now())
	if err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	w.Header().Set("HX-Trigger", "kanban:commented")
	s.write(w, "card.comments.html", filed.Card)
}

// handleCardComments reads one Card's Comments region on its own: the same fragment a filed Comment answers
// with, so a modal whose ordinal went stale - somebody else filed one first - can re-read just that region
// and leave the edit form, and whatever the human has typed in it, alone.
func (s *Server) handleCardComments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	card, err := s.readCardFor(id)
	if err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	s.write(w, "card.comments.html", card)
}

// readCardFor is what every door that renders a Card needs before it renders it: the lookup of the file holding
// the id, the parse into a Card read out of the Column it sits in, and nothing else - the refusal it fails at
// keeps its kind, so a door answers 404 for an id no Card holds and 500 for a file that stopped being a Card, and
// the whole Card view adds its own done question on top of this.
func (s *Server) readCardFor(id string) (Card, error) {
	col, _, raw, err := s.board.cardFile(id)
	if err != nil {
		return Card{}, err
	}
	return readCard(raw, col, id, time.Now())
}

// handleCardStep is the Card modal's half of the step door: it files the step the field names, marks the rung a box
// sits on done, opens one that was marked with op=undo, and erases the step a rung's glyph asks for. It is the
// hand's half of Board.Step, so the refusals that reach this route are the ones a hand is given: no step verb at all
// on a Card in closed, and none on a Card a session is holding in doing - which is the control's own disabled state
// said a second time.
//
// It answers the smallest thing the write changed, and it fires no kanban:saved: working a step does not finish with
// the Card, which is the same reason a filed Comment leaves the modal open, and the count and the bar behind the
// dialog are the next poll's to redraw. A mark, an opening, or a control stale about a step
// already in the state it asked for answers with the one rung it moved, which is what an empty rung costs and leaves
// the words a human is typing into the filing field where they were; a filing, an erase, or a name the Card already
// files changed the ladder's shape and answers the whole region, carrying the count, whether each control is live,
// the words a locked Card paints instead of controls, the error slot a refusal fills, and the field itself.
func (s *Server) handleCardStep(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ask, err := requestedStep(r)
	if err != nil {
		reject(w, http.StatusBadRequest, err)
		return
	}
	// The Card is read before it is written into, as every door that renders one reads it: a file that stopped
	// being a Card is named before a step is added to it rather than after.
	if _, err := s.readCardFor(id); err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	touched, err := s.board.Step(StepRequest{Card: id, Text: ask.Text, Ordinal: ask.Ordinal, Op: ask.Op}, time.Now())
	if err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	if answersLadder(touched.Did) {
		s.write(w, "card.steps.html", ladderOf(touched.Card))
		return
	}
	s.write(w, "card.step.html", rungOf(touched.Card, touched.Step))
}

// ladderView is what a Card's step fragments are drawn from: the Card the ladder belongs to, and the rungs this one
// answer paints.
type ladderView struct {
	Card  Card
	Rungs []rungView
}

// rungView is one rung drawn on one Card: the rung's control posts to that Card's step door and asks whether that
// Card is worked at, so the Card travels with each rung beside it rather than only above the ladder.
type rungView struct {
	Card Card
	Rung stepRung
}

// ladderOf is a Card's whole ladder as the page draws it, spent rungs included.
func ladderOf(c Card) ladderView {
	spent := cardLadder(c)
	rungs := make([]rungView, 0, len(spent))
	for _, r := range spent {
		rungs = append(rungs, rungView{Card: c, Rung: r})
	}
	return ladderView{Card: c, Rungs: rungs}
}

// rungOf is the one rung a step write moved, drawn as the Card would draw it.
func rungOf(c Card, s Step) ladderView {
	return ladderView{Card: c, Rungs: []rungView{{Card: c, Rung: stepRung{Step: s}}}}
}

// answersLadder is which step writes answer the Card's whole ladder and which answers one rung of it. A filing, an
// erase, or a name the Card already files changed the ladder's shape; a mark, an opening, or a control that asked
// for a state the step already held moved one rung.
func answersLadder(did string) bool {
	switch did {
	case stepFiled, stepErased, stepAlreadyFile:
		return true
	}
	return false
}

// stepFieldHint is the tooltip a Card's filing field carries: what this Card is out of room on. A Card holding every
// live step is out of room for a name as much as for another rung, so both readings name the same limit.
func stepFieldHint(c Card) string {
	if stepsFull(c) {
		return fmt.Sprintf("%d steps max.", maxLiveSteps)
	}
	return fmt.Sprintf("a step names one small task, at most %d characters.", stepNameLimit)
}

// handleCardDelete takes a Card off the board: its file moves into .kanban/trash/ and the board stops
// listing it. The browser has already put the human's own answer between the click and this
// request - hx-confirm on the button in card.detail.html - so this door does one thing and sends nothing
// back to swap: 204 with HX-Trigger: kanban:saved, which is what closes the modal the button sits in and
// lets the next poll redraw the Column the Card was in. It is the one Card write with no response body,
// because the Card it names is no longer there to render - and it is refused outright for a Card in done or
// in closed, because finished work is closed rather than deleted and closed rather than reopened.
func (s *Server) handleCardDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.board.Delete(id); err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	w.Header().Set("HX-Trigger", "kanban:saved")
	w.WriteHeader(http.StatusNoContent)
}

// handleCardClose is the modal's door onto the record: it takes a done Card into .kanban/closed/, which the
// board keeps rather than shows. Only a Card in done answers it, because closed says "this work is
// over and is kept" while the file records no Column of its own. The answer is the one every accepted write
// gives - the Columns it changed, each in its own <hx-partial> - with the closed one only when the page was
// drawing it, and kanban:saved is what closes the modal the button was pressed in, the same event a delete
// answers with.
func (s *Server) handleCardClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.board.Close(id); err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	s.writeColumns(w, r, []string{DoneColumn, ClosedColumn})
}

// handleCardReopen is the way back out of closed: the Card is filed at the end of done's Order, so the work
// that has to be worked again is what a reader of that Column sees first. It answers a
// Card sitting in closed and nothing else - a Move into done is the door a commit walks through, and Restore
// is the undo of a delete - so it is a human's door like closing, and it answers both Columns for the same
// reason closing does.
func (s *Server) handleCardReopen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.board.Reopen(id, time.Now()); err != nil {
		reject(w, refusalStatus(err), err)
		return
	}
	s.writeColumns(w, r, []string{ClosedColumn, DoneColumn})
}

// refusalStatus is the whole reading of a Card write the Board refused: its four kinds, one status apiece.
// Bad input is what no re-read makes right - a Column the board does not have, a done move with no sha, a
// Comment with no words - so 400. An id no Card file holds is 404, the answer a stale tab produces. A refusal
// that stands because of what the board is holding - another session's Claim, a name another Card has taken,
// a Comment a hand has answered, a Card that moved mid-write, a Card sitting in a Column this door does not
// answer - is 409, because the request is one the board may well answer once the human has re-read it. What no
// site named is the board failing at its own job - a Card file it cannot read, a Card file it cannot parse, a
// write that did not land - and that is 500: calling a broken Card file bad input sends the human to look in
// the wrong place.
//
// It reads the kind the Board named rather than picking through sentinels here, so a refusal a verb gains
// travels with the kind it was decided as instead of falling into whatever this door happened to list. A door's
// own bad input - a body that is not JSON, a field this route does not take - is still written as 400 at the
// door, because that is the door's own fact to state.
func refusalStatus(err error) int {
	switch kindOf(err) {
	case RefusalBadInput:
		return http.StatusBadRequest
	case RefusalNoCard:
		return http.StatusNotFound
	case RefusalConflict:
		return http.StatusConflict
	default: // RefusalBoardFailure, and any kind this map has not been given yet
		return http.StatusInternalServerError
	}
}

// requestedComment reads the two fields a Comment arrives with: its text, and comment_id, the ordinal this
// Comment means to take - the hidden field the modal fills in, absent or empty meaning "the next one". A form
// body is what htmx sends and JSON is what curl writes, like every other Card write here. It is also where
// a request that could never be a Comment is answered: text the modal left empty, or a number that is no
// ordinal anywhere. A door names its own bad input, so the model layer's refusals below it read as what they
// are - a conflict, or the board failing at its own job.
func requestedComment(r *http.Request) (string, int, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Text      string `json:"text"`
			CommentID int    `json:"comment_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return "", 0, fmt.Errorf(`body is not a JSON object with "text" and an optional "comment_id": %w`, err)
		}
		return commentRequest(in.Text, in.CommentID)
	}
	if err := r.ParseForm(); err != nil {
		return "", 0, fmt.Errorf("body is not a form or a JSON object: %w", err)
	}
	wants := 0
	if raw := strings.TrimSpace(r.PostForm.Get("comment_id")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return "", 0, fmt.Errorf("comment_id %q: want a whole number", raw)
		}
		wants = n
	}
	return commentRequest(r.PostForm.Get("text"), wants)
}

// commentRequest is the reading both bodies share: the text, trimmed, and the ordinal it names. Nothing here
// is a Comment the board could file - empty words, or a number that is not an ordinal - so the door says so
// rather than passing the request down to be refused as something else.
func commentRequest(text string, wants int) (string, int, error) {
	if text = strings.TrimSpace(text); text == "" {
		return "", 0, fmt.Errorf("a Comment needs text")
	}
	if wants < 0 {
		return "", 0, fmt.Errorf("comment_id %d: want a whole number 0 or above", wants)
	}
	return text, wants, nil
}

// requestedStep reads what one of the modal's step controls arrives with: the ordinal of the step the rung was drawn
// on, the op that control's own state names - absent when the box was open and the click means "mark this one done",
// undo when it was ticked and the click means "open this one again", erase when the rung's glyph was pressed - and
// the name the filing field holds. A form body is what htmx sends and JSON is what curl writes, like every other
// Card write here, and a body that could never be a step is answered at the door rather than passed down to be
// refused as something else.
func requestedStep(r *http.Request) (stepAsk, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			Ordinal int    `json:"ordinal"`
			Op      string `json:"op"`
			Text    string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return stepAsk{}, fmt.Errorf(`body is not a JSON object with "ordinal", "text" and an optional "op": %w`, err)
		}
		return stepControlRequest(in.Ordinal, in.Op, in.Text)
	}
	if err := r.ParseForm(); err != nil {
		return stepAsk{}, fmt.Errorf("body is not a form or a JSON object: %w", err)
	}
	ordinal := 0
	if raw := strings.TrimSpace(r.PostForm.Get("ordinal")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return stepAsk{}, fmt.Errorf("ordinal %q: want a whole number", raw)
		}
		ordinal = n
	}
	return stepControlRequest(ordinal, strings.TrimSpace(r.PostForm.Get("op")), r.PostForm.Get("text"))
}

// stepAsk is the reading both bodies share: which rung, which op, and what name to file.
type stepAsk struct {
	Ordinal int
	Op      string
	Text    string
}

// stepControlRequest is the reading both bodies share. The ops the page draws controls for are the page's facts to
// name, and a filing field carries no op at all; an ordinal is needed only when no name is named. Everything about
// the step itself - a name too long, a name on a rung an ordinal names, an ordinal this Card does not hold - is
// refused by the model door in the words it uses at every door.
func stepControlRequest(ordinal int, op, text string) (stepAsk, error) {
	switch op {
	case "", stepUndo, stepErase:
	default:
		return stepAsk{}, fmt.Errorf(`step op %q: this door marks a step done, opens it with %q, or erases it with %q`, op, stepUndo, stepErase)
	}
	text = strings.TrimSpace(text)
	if text == "" && ordinal <= 0 {
		return stepAsk{}, fmt.Errorf(`a step control names "ordinal", the ordinal of the step on this Card, or "text" for the step to file`)
	}
	return stepAsk{Ordinal: ordinal, Op: op, Text: text}, nil
}

// reject answers a failed board write in plain text, because a refusal the browser would swap into a
// column is worse than one it reports beside the board (see web/board.js).
func reject(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprint(w, err.Error())
}

// handleStatic serves the stylesheet, the vendored htmx and board.js. All are embedded in the binary,
// so the board needs no network and no build step.
//
// The three are revalidated rather than refetched. http.ServeContent answers If-None-Match only against an
// ETag, and it derives one from a file's modification time, which an embed.FS reports as zero; the board
// therefore names each asset by its own bytes, hashed on the way out. A browser holding the copy it already
// has pays a request for it rather than htmx's 104 KB, which is what makes a board reached through an ssh
// tunnel worth fetching at all, while a restarted extension serves different bytes and is authoritative for
// the stylesheet. That reaches as far as the assets: a document the browser is holding keeps the footer it was
// written with until a human re-reads it, which is why the page stamps its version and the poll notices a
// board answering another one.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(sub, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sum := sha256.Sum256(data)
	w.Header().Set("ETag", fmt.Sprintf("%q", hex.EncodeToString(sum[:8])))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, sub, name)
}

// underPath reports whether a path sits at or inside a directory, compared as paths rather than as
// prefixes, so /work/repo-extra is not under /work/repo.
func underPath(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// The board names the install serving it, because the words a human types to update one copy are
// different for the other: the copy under <project>/.zot/extensions is the one an install run from the
// project moves, `zot ext install` the global one.
const (
	installLocal  = "local"
	installGlobal = "global"
	installRepo   = "repo"
)

// installKind names the copy whose code is answering: the project-local install under
// <project>/.zot/extensions, the global one under zot's state directory, or neither - the tree itself,
// which is what `zot --ext .` loads, and what a `zot-kanban serve` run out of a repository build is.
func installKind(project, installDir string) string {
	switch {
	case underPath(installDir, filepath.Join(project, ".zot", "extensions")):
		return installLocal
	case underPath(installDir, filepath.Join(zotHome(), "extensions")):
		return installGlobal
	default:
		return installRepo
	}
}

// zotHome is zot's resolved state directory, where `zot ext install` puts a global copy: the ZOT_HOME
// override first, then $XDG_STATE_HOME/zot on any platform, then that platform's own state directory
// (zot's own extension docs, installed beside zot as $ZOT_HOME/docs/extensions.md, "Layout & discovery"). It
// is derived here only to tell a global install from an
// explicit --ext path, and a wrong guess answers "repo" rather than being wrong about anything else.
func zotHome() string {
	if h := os.Getenv("ZOT_HOME"); h != "" {
		return h
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "zot")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "zot")
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "zot")
	default:
		return filepath.Join(home, ".local", "state", "zot")
	}
}

// write renders one template. A board that cannot be read is still a board rendered with an error
// band inside it (see data in view.go), so it answers 200 and the polling browser shows the failure. A template
// that cannot render at all is a different failure and answers 500, because showing an empty board
// would look like an empty board.
func (s *Server) write(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "board render failed: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}
