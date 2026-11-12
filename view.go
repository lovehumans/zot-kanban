package main

import (
	"net/http"
	"strings"
)

// view.go is the data the board page is drawn from: the view a request asked the page to show, the Cards
// that view reaches, and the columns and counts the page prints. It is the page's own half and not a second
// model: the filesystem under .kanban/ is the state the page presents, every Card here comes from
// the Board's one read of it, and the answers a page prints about a Column or a Comment are the Board's
// answers quoted as calls - ruleFor, movable and nextCommentID in NewServer's FuncMap - never restated here
// as fields of their own.
//
// The map keys named below are the contract with the templates. board.html reads .CWD, .Install, .Total,
// .Shown, .Closed, .ClosedView, .Query and .Version; index.html reads .Columns, .Error and .Version;
// column.fragment.html reads .Name and .Cards off each entry of .Columns, and prints a Card by handing it to
// card.fragment.html. A key renamed here is a template that prints nothing, with nothing in Go to say so.
// That is why the page is still drawn from a map rather than from typed column and card views: a
// struct would turn such a rename into a render failure, and typed views would take over the readings the model
// layer calls "a reading of the Card and not a field of its file" - Contradiction, Unseen, UnseenComments and
// NamesHolder - which today travel on the Card type as fields no Card file has.

// filterCards is the board's answer to a query: the Cards every word of it reaches. Words are asked of the
// Card, not of one field of it, so they may land anywhere on it - the title, the id, the body, a Comment an
// Agent filed - and a Card carrying all of them is kept. A word is a substring, and case is not a
// distinction. A query of nothing keeps everything: the filter is what a request asks for, never something
// the board carries.
func filterCards(cards []Card, query string) []Card {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return cards
	}
	var kept []Card
	for _, c := range cards {
		if cardReaches(c, words) {
			kept = append(kept, c)
		}
	}
	return kept
}

// cardReaches says whether every word appears somewhere on this Card. Its fields are read as one text
// because the words of one query are one question about one Card, not one question per field of it.
func cardReaches(c Card, words []string) bool {
	found := []string{c.ID, c.Title, c.Body}
	for _, cm := range c.Comments {
		found = append(found, cm.Text)
	}
	whole := strings.ToLower(strings.Join(found, "\n"))
	for _, w := range words {
		if !strings.Contains(whole, w) {
			return false
		}
	}
	return true
}

// pageView is the view a request asked the page to show: the filter's words, and whether the Column of closed
// Cards is drawn beside the six. Both arrive the same way - ?q= and ?closed=1 on a GET, a q field and a closed
// field in the body of a write - because web/board.js sends the page's view with every request the page makes:
// a closed Column a poll did not ask for would vanish 3 s after a human asked to see it, and a Column a write
// re-renders would widen back to everything it holds. A write is free to name a value no Card has and
// requestedFields ignores it, so a view never makes a legal write illegal.
type pageView struct {
	Query  string
	Closed bool
}

// viewOf reads that view off a request. closed is the flag rather than a Card field: the Board holds closed
// Cards in one directory and a Card's file records no Column of its own, so what a request names is which
// Columns the page draws, not which Cards to keep.
func viewOf(r *http.Request) pageView {
	if err := r.ParseForm(); err != nil {
		// The reason a body cannot be parsed is the handler's to report; a view is not why a request fails.
		return pageView{}
	}
	return pageView{Query: strings.TrimSpace(r.Form.Get("q")), Closed: r.Form.Get("closed") == closedViewFlag}
}

// closedViewFlag is the only value that turns the closed Column on, so a stray closed= or closed=true does not.
const closedViewFlag = "1"

// data groups cards into columns in board order. Column names are never request data: they come
// from Columns, plus the one Column the view may ask for, so no request can name a directory the board
// renders. A query narrows what the board shows without narrowing what it holds, so the footer can say how
// much of the board is on the screen.
func (s *Server) data(cards []Card, loadErr error, view pageView) map[string]any {
	// closed Cards are walked and painted only when the view asks for them, and when it does not they join
	// neither Total nor Shown: the number the footer prints keeps meaning the Columns the board renders.
	var kept, closedCards []Card
	for _, c := range cards {
		if c.Column == ClosedColumn {
			closedCards = append(closedCards, c)
			continue
		}
		kept = append(kept, c)
	}
	shown := filterCards(kept, view.Query)
	columns := make([]map[string]any, 0, len(Columns)+1)
	for _, name := range Columns {
		columns = append(columns, s.column(name, shown))
	}
	total, onScreen := len(kept), len(shown)
	if view.Closed {
		// The closed Column is the board's record, so it sits at the right end of what the page draws: the six
		// keep their order and the seventh is the one a human asked to see. Inside it a Card the query does not
		// reach is hidden as it is anywhere else on the page, and a Card it does reach joins the counts the
		// footer prints, because from here it is on the screen.
		closedShown := filterCards(closedCards, view.Query)
		columns = append(columns, s.column(ClosedColumn, closedShown))
		total += len(closedCards)
		onScreen += len(closedShown)
	}
	out := map[string]any{
		"CWD": s.cwd,
		// Which copy of this extension is answering. Its files sit in the project; the code serving them may
		// sit anywhere, and the two installs on one machine are moved by different commands.
		"Install": s.install,
		"Columns": columns,
		"Total":   total,
		"Shown":   onScreen,
		// The Cards the board keeps closed, said as a number of its own rather than folded into the two above:
		// it is what the page holds in .kanban/closed/, with or without the view drawing them, and it is the
		// words on the link that turns that Column on.
		"Closed":     len(closedCards),
		"ClosedView": view.Closed,
		// The filter box shows what the board is filtering by, so a link that does carry ?q= arrives already
		// filtered and says so on its face.
		"Query": view.Query,
		// The board a browser is looking at is only as new as the binary serving it: the stylesheet, the
		// scripts and the routes are all embedded. So the page says which version it is, in the same words
		// zot-kanban version and zot ext list report.
		"Version": version,
	}
	if loadErr != nil {
		// A board that cannot be read is shown as an error band inside the board, not as a status
		// code a polling browser would discard: the human sees which file is broken.
		out["Error"] = loadErr.Error()
	}
	return out
}

func (s *Server) column(name string, cards []Card) map[string]any {
	var held []Card
	for _, c := range cards {
		if c.Column == name {
			held = append(held, c)
		}
	}
	return map[string]any{"Name": name, "Cards": held}
}
