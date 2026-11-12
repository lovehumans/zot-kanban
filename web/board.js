// Moving a Card on the board: native HTML5 drag-and-drop, with htmx for the request and the swap.
// The card carries data-card, the column carries data-column, and a column marked data-no-drop is not
// a target - that is the board saying done cannot be reached by dragging and closed cannot either,
// because the only way out of closed is reopening and the only way in is a close. A drop names where it
// landed: the Card the dotted line pointed at, or "end" for after the Column's last Card, and that is what
// puts a Card in place - within a Column as well as between two of them.
(function () {
  "use strict";
  var dragging = null, dragID = "", over = null, line = null;

  function columnOf(el) { return el ? el.closest("[data-column]") : null; }
  function clearLine() { if (line) { line.remove(); line = null; } }
  // The reason beside the board, and the one clock on it. #move-error sits outside #board on purpose - the
  // 3 s poll cannot pull it out from under a drag - which is also how it outlives one: the same poll
  // re-rendering a Column mid-drag is what leaves a drag with no dragend to take the words back (the
  // human's zen-browser run, 2026-11-07). So the words keep their own expiry: long enough to read twice,
  // short enough that a board is not still explaining a drag that finished minutes ago. Only this slot
  // expires - a reason inside the modal or the composer sits beside text the human is still holding in a
  // form, and that form's next write is what takes it off.
  var NOTE_MS = 10000, noteTimer = null;
  function say(reason) {
    var note = noteOf();
    if (!note) return;
    note.textContent = reason;
    if (noteTimer) { clearTimeout(noteTimer); noteTimer = null; }
    if (reason) noteTimer = setTimeout(clearNote, NOTE_MS);
  }
  function clearNote() {
    if (noteTimer) { clearTimeout(noteTimer); noteTimer = null; }
    var note = noteOf();
    if (note) note.textContent = "";
  }
  // Everything a drag paints on the way - the line, the Column it is over, the reason beside the board -
  // comes off together. The classes are swept from the page rather than taken off the one node `over`
  // remembers, because the 3 s poll re-renders a Column mid-drag and a Column that refused a drag is where
  // a stale edge outlives the drag (the human's zen-browser run, 2026-11-07).
  function clearOver() {
    clearLine();
    document.querySelectorAll(".col-dragover, .col-refused").forEach(function (el) {
      el.classList.remove("col-dragover", "col-refused");
    });
    over = null;
    clearNote();
  }
  function noteOf() { return document.getElementById("move-error"); }

  // The filter the human is looking through. It is one value the whole page shares, and every request that
  // re-renders a region has to name it: the poll and the forms carry it through hx-include:inherited on the
  // page body (web/board.html), and the drag below is the one request this file issues by itself.
  function filterEl() { return document.getElementById("q"); }
  function filterValue() { var box = filterEl(); return box ? box.value : ""; }

  // Which view the page is in. The closed Column is on the page only because a request put it there, and a
  // write that re-renders a Column has to re-render it in that same view. web/board.html keeps the
  // fact in a hidden field that exists only on a page drawing closed, and hx-include carries it on every
  // request a template makes; this file's drag is the one request that is not made by a template, so it names
  // the view here too. An empty value is the same as naming nothing, which is what a board not showing closed
  // sends.
  function closedValue() { return document.getElementById("closed-state") ? "1" : ""; }

  document.addEventListener("dragstart", function (e) {
    var card = e.target.closest("[data-card]");
    if (!card) { e.preventDefault(); return; }
    clearOver(); // a drag starts clean: the last one's reason is not carried into this one
    dragging = card;
    dragID = card.dataset.card;
    e.dataTransfer.effectAllowed = "move";
    // The id travels in the drag, not in the element: the 3 s poll can swap the board mid-drag and
    // detach the node being dragged.
    e.dataTransfer.setData("text/plain", dragID);
  });

  document.addEventListener("dragend", function () { dragging = null; dragID = ""; clearOver(); });

  // Where a drop would land inside a Column: the Card whose middle is below the pointer is the one the
  // dragged Card lands above, and a pointer below every middle is after the Column's last Card. The Card
  // being dragged is not its own neighbour - and after a poll swap it is the id rather than the node that
  // says which Card that is.
  function placeIn(col, clientY) {
    var cards = col.querySelectorAll("[data-card]");
    for (var i = 0; i < cards.length; i++) {
      if (cards[i].dataset.card === dragID) continue;
      var box = cards[i].getBoundingClientRect();
      if (clientY < box.top + box.height / 2) return { above: cards[i].dataset.card, before: cards[i] };
    }
    return { above: "end", before: null };
  }

  // The dotted line a drop lands on. It joins the Column the drag is over and leaves when the drag is
  // anywhere else, so it is never a Card, never a drop target and never part of what a poll swaps in.
  function showLine(col, where) {
    clearLine();
    line = document.createElement("div");
    line.className = "insert-at";
    if (where.before) col.insertBefore(line, where.before);
    else col.appendChild(line);
  }

  // What a Column nothing lands in says while a drag is over it. dragover is the only event such a Column
  // ever gets: without preventDefault the browser never offers a drop, so no request is made and no refusal
  // text ever arrives - the rule would be enforced in silence.
  // The words are the sentence the Column prints
  // under its own name (ruleFor, the Board's own answer, printed by web/column.fragment.html), said a
  // second time in #move-error, which sits outside #board so the 3 s poll cannot take it away, and on the
  // edge of the Column the pointer is actually over. clearOver - what dragend, dragstart and a drag that
  // leaves the Columns all call - puts both back.
  function sayNoDrop(col) {
    if (over !== col) { clearOver(); over = col; col.classList.add("col-refused"); }
    var why = col.querySelector(".col-rule");
    if (why) say(why.textContent);
  }

  document.addEventListener("dragover", function (e) {
    var col = columnOf(e.target);
    if (!dragging) return;
    if (!col) { clearOver(); return; } // between the Columns, or off the board: nothing is being aimed at
    if (col.hasAttribute("data-no-drop")) { sayNoDrop(col); return; }
    e.preventDefault(); // without this the browser never offers a drop
    e.dataTransfer.dropEffect = "move";
    if (over !== col) { clearOver(); over = col; col.classList.add("col-dragover"); }
    showLine(col, placeIn(col, e.clientY));
  });

  document.addEventListener("drop", function (e) {
    var col = columnOf(e.target);
    if (!dragging || !col || col.hasAttribute("data-no-drop")) return;
    // The place is read before the Column is unwound, because it is the pointer's position that says it.
    var where = placeIn(col, e.clientY);
    clearOver();
    e.preventDefault();
    var id = e.dataTransfer.getData("text/plain") || dragID;
    var to = col.dataset.column;
    dragging = null;
    dragID = "";
    // The response names the regions it updates: each Column it changed arrives in its own
    // <hx-partial>, so a partial swap needs no flag invented in a template. target and swap
    // remain as the fallback for a response that carries a main swap instead. The Column named is the one
    // the drag ended over, which is the one the Card already sits in when the drag only sorted it.
    htmx.ajax("PATCH", "/api/card/" + encodeURIComponent(id), {
      target: "#col-" + to, swap: "outerHTML", values: { column: to, above: where.above, q: filterValue(), closed: closedValue() },
    });
  });

  // A refused write (into done, an id that no longer exists, a field the model refuses, a Card somebody
  // else deleted first, an id two files hold) is shown rather than swapped: board.html tells htmx not to swap
  // 4xx and 5xx at all. Where the reason appears depends on where the human is looking. A Card open in the
  // modal covers the board and leaves it inert, so a reason beside the board would sit behind the dialog; a
  // save that fails says so inside the modal, in the slot card.detail.html carries. A Card being filed from
  // the composer strip has its own slot, because the strip is where the human is typing. The polled region
  // re-renders neither one, and the next write clears it.
  document.addEventListener("htmx:response:error", function (e) {
    var ctx = e.detail && e.detail.ctx;
    // Every door a Card is written through, whichever slot its reason belongs in: PATCH moves or edits one,
    // POST files a Comment or a new Card, DELETE takes one off the board (handleCardDelete in web.go).
    if (!ctx || ["PATCH", "POST", "DELETE"].indexOf(ctx.request.method) < 0) return;
    var source = ctx.sourceElement;
    var slot = "move-error";
    if (source && source.closest("#card-modal")) slot = "card-error";
    else if (source && source.closest("#composer")) slot = "composer-error";
    // Filing a Comment is refused while the human is looking at the Comments region, so its reason belongs
    // under the form that filed it, not beside the edit form above.
    var commentForm = source && endsWith(source.getAttribute("hx-post"), "/comment");
    if (commentForm) slot = "comment-error";
    var note = document.getElementById(slot);
    // Only the strip beside the board keeps a clock of its own (say); a slot inside a form belongs to that
    // form's next write.
    if (note) { if (slot === "move-error") say(ctx.text); else note.textContent = ctx.text; }
    if (commentForm) {
      var area = document.getElementById("card-comment");
      rereadComments(source.getAttribute("hx-post").slice(0, -"/comment".length) + "/comments",
        area ? area.value : "", ordinalIn(source), ctx.text);
    }
  });

  document.addEventListener("htmx:error", function (e) {
    // A poll that never got an answer is not a refusal. A 4xx or 5xx is an answer and the listener above routes its
    // reason to the slot the human is looking at; a fetch that throws - a closed zot session, a stopped `zot-kanban serve`,
    // a tunnel that is down, or one still hanging when its 60 s timeout fires - has no answer to show, and htmx
    // fires htmx:error with the request's ctx but no ctx.response (web/htmx.js), which nothing on this page listens
    // to. So the board a human is staring at can be a page out of a dead process, silent about why nothing they file
    // arrives. The same strip beside the board says which of the two this is; it is re-said on each failed poll, so
    // it stays while the board is out and clears itself on the next one that answers.
    var ctx = e.detail && e.detail.ctx;
    if (!ctx || ctx.response) return; // an answer arrived; that is the listener above's to explain
    say("the board is not answering - the server is offline or wedged. Start it again (`zot-kanban serve`, or a zot " +
        "session); this page follows on its own");
  });

  function endsWith(haystack, needle) {
    return typeof haystack === "string" && haystack.slice(-needle.length) === needle;
  }

  // The ordinal a Comment form carries came from the read that rendered it. If another hand filed a Comment
  // first, that number is taken and the filing is refused, and the form cannot be retried until it knows the
  // real next ordinal. So the Comments region is re-read on its own - the edit form above it and the text the
  // human typed in it are left alone - and the typed text is put back into the fresh form instead of being
  // thrown away with the stale one.
  // What this door says when the Card moved under the form the human was filling. The server answers a
  // taken ordinal with the whole refusal - which Comment holds it and what it says - which is the right
  // answer for a request written by hand and the wrong one for a person staring at the Comments they were
  // typing into. What they need is that something arrived and their own Comment is one click away.
  var COMMENT_MOVED = "New comment came in meanwhile - post yours anyway?";

  // The ordinal a form carries, which card.comments.html keeps in the hidden comment_id: the number the
  // model layer would assign anyway.
  function ordinalIn(where) {
    var held = where && where.querySelector ? where.querySelector('input[name="comment_id"]') : null;
    return held ? held.value : "";
  }

  function rereadComments(commentsURL, typed, carried, reason) {
    if (!commentsURL) return;
    htmx.ajax("GET", commentsURL, {target: "#card-comments", swap: "outerHTML"}).then(function () {
      // The re-read brings a fresh region, so the reason has to be said again in it, and so does the text.
      // It is said in the door's own words when the fresh region hands back a different ordinal from the one
      // the refused form carried: that difference is the Card moving under the form. A refusal that is not
      // that - an id two files hold - fails this re-read too, so the server's reason is what stands.
      var freshReason = document.getElementById("comment-error");
      if (freshReason) {
        var now = ordinalIn(document.getElementById("card-comments"));
        freshReason.textContent = carried && now && carried !== now ? COMMENT_MOVED : reason;
      }
      var fresh = document.getElementById("card-comment");
      if (fresh && typed) {
        fresh.value = typed;
        fresh.focus();
      }
    }, function () {});
  }

  // Copying a Card's identifier out of the modal. The string the modal prints is the stem of the Card's
  // file, which is what GET and PATCH /api/card/<id> answer to and what the agent tool names a Card with,
  // so pasting it beats retyping a slug built from its title and the minute it was filed. 127.0.0.1 is a
  // secure context, so navigator.clipboard is available; a browser that still refuses the write gets the
  // id selected instead of nothing, and the note says which of the two happened.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var id = btn.dataset.copy;
    function say(text) {
      var note = document.getElementById("copy-note");
      if (note) note.textContent = text;
    }
    function selectID() {
      var printed = document.getElementById("card-id");
      if (!printed || !window.getSelection) return;
      var range = document.createRange();
      range.selectNodeContents(printed);
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
    }
    if (!navigator.clipboard) {
      selectID();
      say("no clipboard in this browser - the id is selected instead");
      return;
    }
    navigator.clipboard.writeText(id).then(function () {
      say("copied");
    }, function () {
      selectID();
      say("the clipboard was refused - the id is selected instead");
    });
  });

  // Opening a Card and dragging one are the two outcomes of the same gesture, on the same element. A
  // modal <dialog> already makes everything behind it inert, so nothing can be dragged out of a column
  // while a Card is open; this is the same rule written where a drag actually starts, and it has to be
  // re-applied because the 3 s poll renders the board's cards with draggable="true" again
  // (probe-htmx-drag-and-drop).
  function modalOf() { return document.getElementById("card-modal"); }
  function setDraggable(on) {
    document.querySelectorAll("[data-card]").forEach(function (card) {
      var col = columnOf(card);
      card.draggable = on && !(col && col.hasAttribute("data-no-drop"));
    });
  }

  document.addEventListener("htmx:after:swap", function () {
    // A page the browser kept is not the board answering. Every dynamic route answers Cache-Control: no-store, so
    // a read that reaches the server is never stale - but a document the browser is holding shows the footer it was
    // written with until a human asks for it again, and the 3 s poll re-renders #board alone. So the
    // polled fragment names the version that wrote it and the document names the version it was written by: when
    // those two differ, the strip beside the board says which two numbers are in play, because the fix is a reload
    // only the human can ask for (the human's Ctrl+Shift+R, 2026-11-10). The strip is re-said on every
    // poll while they differ, so it is a band that stays rather than one that blinks.
    var board = document.getElementById("board");
    var stamped = document.documentElement.dataset.version;
    var served = board && board.dataset.version;
    if (!stamped || !served || stamped === served) return;
    say("board answering " + served + ", this page was written by " + stamped +
        " - reload it; if the footer stays, reload it hard");
  });

  document.addEventListener("htmx:after:swap", function (e) {
    var modal = modalOf();
    if (!modal) return;
    var ctx = e.detail && e.detail.ctx;
    // Either this swap just put a Card into the modal, or the poll re-rendered the board while a Card is
    // open. Both mean the board's cards must stop being drag sources until the modal closes.
    if (modal.open || (ctx && ctx.target === modal)) setDraggable(false);
    // A Card opened is a Card seen: the swap that put it in the modal is where the board's mark on an
    // Agent-filed Card is cleared. No control asks for it, and the id the modal prints is the id
    // the door takes. The answer is empty by design, so nothing swaps; the Column behind the modal loses the
    // asterisk on the next poll, in this browser and in every other one looking at this board.
    if (ctx && ctx.target === modal) markSeen();
    // Whatever this swap put down inside the Card view is what the view was filled with, which is what the
    // unsaved-words guard compares against.
    rebaseModal();
  });

  function markSeen() {
    var printed = document.getElementById("card-id");
    var id = printed ? (printed.textContent || "").trim() : "";
    if (!id) return;
    htmx.ajax("POST", "/api/card/" + encodeURIComponent(id) + "/seen", {target: "body", swap: "none"});
  }

  // Closing a Card view with words still in it. Esc and a click beside the modal both run the dialog's cancel
  // steps, and those steps ask the page first and can be stopped (MDN: HTMLDialogElement.cancel_event), while the
  // X is a form with method="dialog" and so runs the close steps instead. Three doors, two guarded:
  // the X stays open on purpose, because its own tooltip already promises that nothing is changed and the board's
  // rules are that a human closes a Card - pressing it is a human choosing to throw the text away.
  var filled = {};

  function watchedFields(view) {
    return Array.prototype.slice.call(view.querySelectorAll("#card-title, textarea#card-body, textarea#card-comment"));
  }

  // What each watched field was filled with, remembered by the node a swap put there. Node identity is what tells
  // a re-filled field from a field a human has been typing into: a swap that replaces a node measures it again,
  // while a swap elsewhere - the poll redrawing #board, a Comment filed into #card-comments while a body edit is
  // open, a save the server refused - leaves the words where they were measured. So only opening a Card, a
  // rendering the server asked for, or a save that worked can clear the guard.
  function rebaseModal() {
    var view = modalOf();
    if (!view) { filled = {}; return; }
    watchedFields(view).forEach(function (field) {
      var held = filled[field.id];
      if (!held || held.node !== field) filled[field.id] = { node: field, words: field.value };
    });
  }

  function unsavedWords() {
    var view = modalOf();
    if (!view) return false;
    return watchedFields(view).some(function (field) {
      var held = filled[field.id];
      return held && held.words !== field.value;
    });
  }

  var modal = modalOf();
  if (modal) {
    modal.addEventListener("close", function () { filled = {}; setDraggable(true); });
    modal.addEventListener("cancel", function (e) {
      if (!unsavedWords()) return;
      if (!window.confirm("Close this Card view? What you typed here has not been saved.")) e.preventDefault();
    });
  }

  // A write the server accepted names itself on the way back: HX-Trigger: kanban:saved (web.go). The
  // Columns it changed have already arrived in <hx-partial> regions, so the only thing left is the modal
  // the save came from - it closes, and the board behind it is already showing the saved Card. A drag
  // answers the same way and has no modal open, so this is a no-op there. A delete answers the same way with
  // no body at all: the Card it names is no longer there to render, so closing the modal is the whole
  // response, and the next poll redraws the Column it left.
  //
  // A filed Comment announces itself as kanban:commented instead, and deliberately has no listener here: a
  // Comment does not finish with the Card, so the modal that filed one stays open, and the region it was
  // filed from is already the response's own swap (web/card.comments.html). The Card's comment count behind
  // the modal is the 3 s poll's to refresh. The announce is still sent because every accepted write names
  // itself, and a listener added here later would be closing a modal the human is still typing in.
  document.addEventListener("kanban:saved", function () {
    var open = modalOf();
    if (open && open.open) open.close();
  });

  // The filter, reached the way a search box on a page you are already reading is reached: "/". It does not
  // fire while a dialog is open, because then the keys belong to that dialog - the Card's or the board's own
  // explanation - and not while the caret is already sitting in something you are typing into, because then "/" is
  // a character of the text. The question asked is any open <dialog>, not the Card modal alone, so a modal added to
  // this page is covered by the rule instead of needing its own clause.
  document.addEventListener("keydown", function (e) {
    if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey) return;
    if (document.querySelector("dialog[open]")) return;
    var typed = e.target;
    if (typed && (typed.tagName === "INPUT" || typed.tagName === "TEXTAREA" ||
                  typed.tagName === "SELECT" || typed.isContentEditable)) return;
    var box = filterEl();
    if (!box) return;
    e.preventDefault();
    box.focus();
  });

  // The keys that write a form write the form the caret is in: Ctrl+S saves the Card open in the modal, while
  // Ctrl+Enter files what the human is typing - a Card in the composer strip, a Comment in the modal (Cards and ).
  // One rule for both keys and for the board's
  // three forms, rather than three bindings to three widget ids: the key says *write*, the caret says *which*.
  // Ctrl+S is the browser's own "save this page" key, so it is taken only where a form is in reach and left to the
  // browser where none is; Ctrl+Enter is the send key a textarea wants, where Enter already submits a
  // single-line field. Cmd travels with Ctrl, because that is where the save key sits on a Mac.
  //
  // requestSubmit is what a click on the button does: it runs the field's own validation - the modal's title and
  // the Comment's text are `required` - and fires the submit event htmx answers, so a refusal lands in the slot
  // board.js already places it in.
  document.addEventListener("keydown", function (e) {
    if (e.key !== "s" && e.key !== "Enter") return;
    if (!(e.ctrlKey || e.metaKey) || e.altKey) return;
    var form = formOf(e.target);
    // Ctrl+S with a Card open and the caret nowhere in its form: showModal puts the caret on the dialog's first
    // control, which is its close button, and the human is still asking for this Card to be saved.
    if (!form && e.key === "s") {
      var open = modalOf();
      if (open && open.open) form = open.querySelector("#card-edit");
    }
    if (!form) return;
    e.preventDefault();
    form.requestSubmit();
  });

  // Which form a keystroke belongs to: the one the caret is inside, or the one it is associated with by the HTML
  // form attribute - the modal's title field is printed outside the edit form (card.detail.html) and `el.form` is
  // that association. The two modals' close controls are forms too, and a form whose method is dialog is how a
  // dialog is dismissed rather than a write, so a keystroke beside the close button can neither file what is
  // typed in the Card view nor throw it away.
  function formOf(el) {
    var form = el && el.closest ? el.closest("form") : null;
    if (!form && el) form = el.form;
    if (!form || form.getAttribute("method") === "dialog") return null;
    return form;
  }

  // Closing a Card: the form's button and the Escape key come from the dialog element itself, and a click
  // beside it comes from closedby="any" in web/board.html - a modal dialog's own default leaves light
  // dismiss off (MDN: HTMLDialogElement.closedBy). Chrome's guidance carries a coordinate-check fallback for
  // a browser without the attribute, and this board does not: Firefox has had the attribute since 141, the
  // human's zen-browser sits on Firefox 156+, and Safari is the only browser left, so the shim would be
  // code no test here can reach. On Safari the modal closes by its button or by Esc.

  // A Card's words, read as Markdown. Its body and Comments are plain text in the Card's file, in what the tool
  // reads and in what htmx swaps in, and web/markdown.js is what turns them into HTML here and only here.
  // Two rules hold this in place. The words travel
  // once: what gets rendered is what the swapped node already holds, so a Card read by curl and the same Card in a
  // browser differ only in whether they were read. And a Card is read before it is edited, so the body's reading
  // face is what a click turns back into the field - the textarea stays the one thing the form submits, and it is
  // where the words live while the page is showing them. The composer's own body field is left alone: a field a
  // human is typing into shows the Markdown they are writing, not a preview of it.
  function renderBody(field) {
    var view = document.createElement("div");
    view.id = "card-body-view";
    view.className = "card-body md";
    view.tabIndex = 0;
    view.title = "click to edit this body";
    function edit() {
      view.hidden = true;
      field.hidden = false;
      field.focus();
    }
    function read() {
      var html = renderMarkdown(field.value);
      if (!html) { // a body holding no words has nothing to read, so the field stays the face it can type into
        view.hidden = true;
        field.hidden = false;
        return;
      }
      view.innerHTML = html;
      field.hidden = true;
      view.hidden = false;
    }
    view.addEventListener("click", edit);
    // The reading face is in the tab order, so the keys a click answers there answer it too.
    view.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); edit(); }
    });
    // Leaving the field puts the words back into their reading face. Saving is the form's own business - Ctrl+S
    // and the save button read the field whether it is showing or not.
    field.addEventListener("blur", read);
    read();
    field.parentNode.insertBefore(view, field);
  }

  // Rendering happens where a swap put the words down, which is why it is keyed off the swap rather than off the
  // modal opening: the Card the modal shows arrives in one swap, and the region a filed Comment answers with is
  // another. data-md marks a node this pass has already read, because a Comment filed into the region re-renders
  // the region and re-rendering words that are already HTML would read the markup a code span was holding as
  // Markdown a second time.
  function renderCardText(root) {
    root.querySelectorAll(".comment-text:not([data-md])").forEach(function (node) {
      node.setAttribute("data-md", "1");
      // The class comes with the rendering rather than sitting in the template, because the sheet holds the raw
      // words in pre-wrap until this pass runs: a Card read with no JavaScript is still the plain Markdown it is.
      node.classList.add("md");
      node.innerHTML = renderMarkdown(node.textContent);
    });
    var field = root.querySelector("textarea#card-body");
    if (field && !document.getElementById("card-body-view")) renderBody(field);
  }

  document.addEventListener("htmx:after:swap", function (e) {
    renderCardText((e.detail && e.detail.target) || document);
  });
})();
