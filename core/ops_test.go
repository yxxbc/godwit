package core

import (
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/internal/testmail"
	"github.com/godwit-email/godwit/core/mailsync"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onServer lists the folder's messages in UID order as "Message-ID flags".
func (e *syncEnv) onServer(t *testing.T, folder string) []string {
	t.Helper()
	c := e.dial(t)
	sel, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	require.NoError(t, err)
	out := []string{}
	if sel.NumMessages == 0 {
		return out
	}
	var all imap.UIDSet
	all.AddRange(1, 0)
	msgs, err := c.Fetch(all, &imap.FetchOptions{UID: true, Flags: true, Envelope: true}).Collect()
	require.NoError(t, err)
	slices.SortFunc(msgs, func(a, b *imapclient.FetchMessageBuffer) int { return int(a.UID) - int(b.UID) })
	for _, m := range msgs {
		var fl []string
		for _, f := range m.Flags {
			fl = append(fl, string(f))
		}
		slices.Sort(fl)
		out = append(out, strings.TrimSpace(m.Envelope.MessageID+" "+strings.Join(fl, " ")))
	}
	return out
}

func (e *syncEnv) folderID(t *testing.T, name string) int64 {
	t.Helper()
	return store.Int(e.rows(t, "SELECT id FROM folders WHERE account_id = ? AND name = ?", e.acc.ID, name)[0][0])
}

func (e *syncEnv) ops(t *testing.T) []Op {
	t.Helper()
	ops, err := e.s.PendingOps(bg, e.acc.ID)
	require.NoError(t, err)
	return ops
}

func (e *syncEnv) opStates(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, v := range e.rows(t, "SELECT state FROM ops ORDER BY id") {
		out = append(out, store.Str(v[0]))
	}
	return out
}

// reopen closes the service and opens a new one on the same data directory.
func (e *syncEnv) reopen(t *testing.T) {
	t.Helper()
	require.NoError(t, e.s.Close())
	s, err := New(bg, Options{DataDir: e.dir, ImageClient: loopbackImageClient()})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	e.s = s
}

func (e *syncEnv) putFlagged(t *testing.T, folder, raw string, flags ...imap.Flag) {
	t.Helper()
	_, err := e.im.User.Append(folder, strings.NewReader(raw), &imap.AppendOptions{Flags: flags})
	require.NoError(t, err)
}

func msg(id string) string {
	return eml(id, "bob@example.com", "alice@example.com", "s "+id, day, "body "+id)
}

func TestSetFlagsIsLocalFirst(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	e.im.Stop()
	ch, unsub := e.s.Subscribe()
	defer unsub()

	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`, `\Flagged`}, nil))
	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, []string{`\Flagged`, `\Seen`}, m.Flags)
	ops := e.ops(t)
	require.Len(t, ops, 1)
	assert.Equal(t, "flags", ops[0].Kind)
	assert.Equal(t, "pending", ops[0].State)
	assert.Zero(t, ops[0].Attempts)
	changed := ofType(collect(ch), events.MessagesChanged)
	require.Len(t, changed, 1)
	assert.Equal(t, []int64{id}, changed[0].MessageIDs)

	require.NoError(t, e.s.SetFlags(bg, []int64{id}, nil, []string{`\Seen`}))
	m, _ = e.s.GetMessage(bg, id, false)
	assert.Equal(t, []string{`\Flagged`}, m.Flags)
	deps := e.rows(t, "SELECT id, depends_on FROM ops ORDER BY id")
	assert.Equal(t, deps[0][0], deps[1][1], "second change waits for the first")
}

func TestMoveThenFlagOffline(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.put(t, "INBOX", msg("other@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	archive := e.folderID(t, "Archive")
	e.im.Stop()

	require.NoError(t, e.s.Move(bg, []int64{id}, archive))
	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, archive, m.FolderID)
	e.reopen(t)
	e.im.Restart()
	e.sync(t)

	assert.Equal(t, []string{"other@x"}, e.onServer(t, "INBOX"))
	assert.Equal(t, []string{`m@x \Seen`}, e.onServer(t, "Archive"))
	assert.Equal(t, id, e.idOf(t, "m@x"))
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE "+shownCol+" = ?", archive))
	assert.Empty(t, e.ops(t))
	assert.Equal(t, []string{"done", "done"}, e.opStates(t))
}

func TestMoveWithoutCOPYUID(t *testing.T) {
	for name, opts := range map[string][]testmail.Option{
		"COPY+STORE+UID EXPUNGE":            {testmail.WithoutMove()},
		"COPY+STORE+UID EXPUNGE no COPYUID": {testmail.WithoutMove(), testmail.WithoutCOPYUID()},
		"MOVE no COPYUID":                   {testmail.WithoutUIDPlus()},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSyncEnv(t, opts...)
			e.putFlagged(t, "INBOX", msg("keep@x"), imap.FlagDeleted)
			e.put(t, "INBOX", msg("m@x"))
			e.put(t, "INBOX", msg("other@x"))
			e.sync(t)
			id := e.idOf(t, "m@x")
			archive := e.folderID(t, "Archive")
			e.im.Stop()
			require.NoError(t, e.s.Move(bg, []int64{id}, archive))
			require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
			e.im.Restart()
			e.sync(t) // one round: the flags change that waited for the move's claim goes out too

			assert.Equal(t, []string{`keep@x \Deleted`, "other@x"}, e.onServer(t, "INBOX"))
			assert.Equal(t, []string{`m@x \Seen`}, e.onServer(t, "Archive"))
			assert.Equal(t, id, e.idOf(t, "m@x"))
			r := e.rows(t, "SELECT "+shownCol+", "+serverCol+", flags FROM messages WHERE id = ?", id)
			assert.Equal(t, []any{archive, archive, `\Seen`}, r[0])
			assert.Equal(t, []string{"done", "done"}, e.opStates(t))
		})
	}
}

// storedCopy stores a second row of message id in folder at UID 1, as Gmail's
// All Mail copy of mail archived there is (#506), and returns its id. folder
// must be empty on the server, so a move's COPYUID into it is 1.
func (e *syncEnv) storedCopy(t *testing.T, id, folder int64) int64 {
	t.Helper()
	r, err := e.s.db.Execute(bg, `INSERT INTO messages(account_id, message_id, subject, size, flags, date)
		SELECT account_id, message_id, subject, size, flags, date FROM messages WHERE id = ?`, id)
	require.NoError(t, err)
	_, err = e.s.db.Execute(bg, `INSERT INTO message_locations(message_id, folder_id, shown, uidvalidity, uid, entered_at, date)
		SELECT m.id, f.id, 1, f.uidvalidity, 1, unixepoch(), m.date FROM messages m, folders f WHERE m.id = ? AND f.id = ?`, r.LastInsertID, folder)
	require.NoError(t, err)
	return r.LastInsertID
}

// A COPYUID naming a UID a row already holds in the destination is the same
// server message: the moved row takes its place instead of the push failing on
// UNIQUE(folder_id, uid).
func TestMoveOntoStoredCopy(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	archive := e.folderID(t, "Archive")
	e.storedCopy(t, id, archive)

	require.NoError(t, e.s.Move(bg, []int64{id}, archive))
	e.sync(t)

	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Archive"))
	assert.Equal(t, [][]any{{id, archive, archive, int64(1)}}, e.rows(t, "SELECT id, "+shownCol+", "+serverCol+", "+uidCol+" FROM messages"))
	assert.Equal(t, []string{"done"}, e.opStates(t))
}

// The copy's queued changes are merged with it (ADR 0034) and still reach the
// server after the moved row's: the copy's move to Trash follows the move the
// user made on the moved row.
func TestMoveOntoStoredCopyCarriesItsQueuedChanges(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	archive, trash := e.folderID(t, "Archive"), e.folderID(t, "Trash")
	cp := e.storedCopy(t, id, archive)

	require.NoError(t, e.s.Move(bg, []int64{id}, archive))
	require.NoError(t, e.s.Move(bg, []int64{cp}, trash))
	e.sync(t)

	assert.Empty(t, e.onServer(t, "Archive"))
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Trash"))
	assert.Equal(t, [][]any{{id, trash, trash}}, e.rows(t, "SELECT id, "+shownCol+", "+serverCol+" FROM messages"))
	assert.Equal(t, []string{"done", "done"}, e.opStates(t))
}

// A flags change queued on the copy shows on the moved row as soon as they
// merge, before the push that applies it.
func TestMoveOntoStoredCopyKeepsItsQueuedFlags(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	cp := e.storedCopy(t, id, e.folderID(t, "Archive"))

	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	require.NoError(t, e.s.SetFlags(bg, []int64{cp}, []string{`\Flagged`}, nil))
	e.sync(t)

	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, []string{`\Flagged`}, m.Flags)
	assert.Equal(t, []string{`m@x \Flagged`}, e.onServer(t, "Archive"))
}

// What points at the copy goes to the moved row: its labels even without a
// label_fp (no Message-ID and a cut body or attachments, which the delete
// trigger skips), a reply draft's source (its threading headers) and rule runs.
func TestMoveOntoStoredCopyKeepsWhatPointsAtIt(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	archive := e.folderID(t, "Archive")
	cp := e.storedCopy(t, id, archive) // stored without label_fp
	l, err := e.s.CreateLabel(bg, LabelInput{Name: "Family", Color: "pink", Icon: "heart-3"})
	require.NoError(t, err)
	_, err = e.s.db.Execute(bg, "INSERT INTO message_labels(message_id, label_id, source, active) VALUES (?, ?, 'manual', 1)", cp, l.ID)
	require.NoError(t, err)
	d, err := e.s.NewDraft(bg, e.acc.ID, "reply", cp)
	require.NoError(t, err)
	_, err = e.s.db.Execute(bg, `INSERT INTO rules(position, enabled, doc, created_at, updated_at) VALUES (0, 0, '{}', 0, 0)`)
	require.NoError(t, err)
	_, err = e.s.db.Execute(bg, "INSERT INTO rule_runs(rule_id, message_id, trigger, state, at) VALUES (1, ?, 'new', 'done', 0)", cp)
	require.NoError(t, err)

	require.NoError(t, e.s.Move(bg, []int64{id}, archive))
	e.sync(t)

	assert.Equal(t, [][]any{{id, l.ID, "manual", int64(1)}}, e.rows(t, "SELECT message_id, label_id, source, active FROM message_labels"))
	assert.Equal(t, [][]any{{id}}, e.rows(t, "SELECT source_message_id FROM drafts WHERE id = ?", d.ID))
	assert.Equal(t, [][]any{{id}}, e.rows(t, "SELECT message_id FROM rule_runs"))
}

func TestMoveNeedsMoveOrUIDPlus(t *testing.T) {
	e := newSyncEnv(t, testmail.WithoutMove(), testmail.WithoutUIDPlus())
	e.putFlagged(t, "INBOX", msg("keep@x"), imap.FlagDeleted)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	ch, unsub := e.s.Subscribe()
	defer unsub()
	e.sync(t)
	ops := e.ops(t)
	require.Len(t, ops, 1)
	assert.Equal(t, "failed", ops[0].State)
	assert.Contains(t, ops[0].LastError, "MOVE")
	evs := collect(ch)
	assert.Len(t, ofType(evs, events.SyncFailed), 1)
	assert.Equal(t, e.folderID(t, "INBOX"), store.Int(e.rows(t, "SELECT "+shownCol+" FROM messages WHERE id = ?", id)[0][0]),
		"shown where the server still has it")
	var announced bool
	for _, ev := range ofType(evs, events.MessagesChanged) {
		announced = announced || slices.Contains(ev.MessageIDs, id)
	}
	assert.True(t, announced)
	assert.Equal(t, []string{`keep@x \Deleted`, "m@x"}, e.onServer(t, "INBOX"))
}

func TestClaimAmbiguity(t *testing.T) {
	e := newSyncEnv(t, testmail.WithoutUIDPlus())
	raw := msg("dup@x")
	e.put(t, "INBOX", raw)
	e.put(t, "Archive", raw)
	e.sync(t)
	inbox, archive := e.folderID(t, "INBOX"), e.folderID(t, "Archive")
	orig := store.Int(e.rows(t, "SELECT id FROM messages WHERE "+shownCol+" = ?", inbox)[0][0])
	require.NoError(t, e.s.Move(bg, []int64{orig}, archive))
	ch, unsub := e.s.Subscribe()
	defer unsub()
	e.sync(t)

	assert.Equal(t, []string{"conflict"}, e.opStates(t))
	assert.Equal(t, []string{"dup@x", "dup@x"}, e.onServer(t, "Archive"))
	r := e.rows(t, "SELECT id, "+uidCol+" FROM messages WHERE "+shownCol+" = ? AND "+serverCol+" = ? ORDER BY "+uidCol+"", archive, archive)
	require.Len(t, r, 2, "both server copies stay separate local messages")
	assert.Equal(t, int64(1), store.Int(r[0][1]))
	assert.Equal(t, int64(2), store.Int(r[1][1]))
	assert.Equal(t, 2, e.count(t, "SELECT count(*) FROM messages"))
	assert.Zero(t, e.count(t, "SELECT count(*) FROM messages WHERE "+serverCol+" IS NULL"), "the row awaiting a claim is gone")
	evs := collect(ch)
	assert.Len(t, ofType(evs, events.SyncFailed), 1)
	changed := ofType(evs, events.MessagesChanged)
	require.NotEmpty(t, changed)
	assert.Contains(t, changed[0].MessageIDs, orig)
}

// Two copies arriving on different pages of new mail are as ambiguous as two
// on one: the move is not claimed by the first.
func TestClaimAmbiguityAcrossPages(t *testing.T) {
	defer func(n int) { mailsync.BackfillPage = n }(mailsync.BackfillPage)
	mailsync.BackfillPage = 1
	e := newSyncEnv(t, testmail.WithoutUIDPlus())
	raw := msg("dup@x")
	e.put(t, "INBOX", raw)
	e.put(t, "Archive", msg("old@x"))
	e.sync(t)
	orig := e.idOf(t, "dup@x")
	require.NoError(t, e.s.Move(bg, []int64{orig}, e.folderID(t, "Archive")))
	e.put(t, "Archive", raw) // another client's copy, before the move lands
	e.sync(t)
	assert.Equal(t, []string{"conflict"}, e.opStates(t))
	assert.Equal(t, 3, e.count(t, "SELECT count(*) FROM messages"), "both server copies stay separate local messages")
}

func TestMoveNotReimportedBeforePush(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	archive := e.folderID(t, "Archive")
	e.im.Stop()
	require.NoError(t, e.s.Move(bg, []int64{id}, archive))
	e.im.Restart()

	_, ferr := mailsync.Fetch(bg, e.s.db, e.dial(t), e.acc.ID, e.s.bus, mailsync.FetchOptions{})
	require.NoError(t, ferr) // pull before push
	assert.Equal(t, [][]any{{id, archive}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))

	e.sync(t)
	assert.Equal(t, [][]any{{id, archive}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Archive"))
	assert.Equal(t, []string{}, e.onServer(t, "INBOX"))
}

func TestUncertainMoveNotRepeated(t *testing.T) {
	e := newSyncEnv(t, testmail.WithoutUIDPlus())
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	// The MOVE went out and reached the server, then the connection dropped.
	_, err := e.s.db.Execute(bg, "UPDATE ops SET attempts = 1")
	require.NoError(t, err)
	c := e.dial(t)
	_, err = c.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	_, err = c.Move(imap.UIDSetNum(1), "Archive").Wait()
	require.NoError(t, err)
	before := e.im.MovesAndCopies()

	e.sync(t)
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Archive"))
	assert.Equal(t, [][]any{{id}}, e.rows(t, "SELECT id FROM messages"))
	assert.Equal(t, []string{"done"}, e.opStates(t))
	assert.Equal(t, before, e.im.MovesAndCopies(), "the MOVE is not sent again")
}

// Without MOVE, a retry after COPY and STORE went through but UID EXPUNGE did
// not finishes the move: it does not COPY a second time.
func TestUncertainCopyMoveNotDuplicated(t *testing.T) {
	e := newSyncEnv(t, testmail.WithoutMove())
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	e.im.DropOnNextUIDExpunge()
	_ = e.s.SyncNow(bg, e.acc.ID) // the connection drops before the EXPUNGE
	e.sync(t)
	e.sync(t) // the destination's copy is claimed
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Archive"))
	assert.Empty(t, e.onServer(t, "INBOX"))
	assert.Equal(t, [][]any{{id}}, e.rows(t, "SELECT id FROM messages"))
	assert.Equal(t, []string{"done"}, e.opStates(t))
}

// \Deleted from another client is no proof an interrupted COPY went through:
// with no copy in the destination, the retry copies it there.
func TestUncertainCopyMoveDeletedElsewhere(t *testing.T) {
	e := newSyncEnv(t, testmail.WithoutMove())
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	// The last attempt failed before its COPY; another client then set \Deleted.
	_, err := e.s.db.Execute(bg, "UPDATE ops SET attempts = 1")
	require.NoError(t, err)
	c := e.dial(t)
	_, err = c.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	require.NoError(t, c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close())
	e.sync(t)
	e.sync(t)
	assert.Equal(t, []string{`m@x \Deleted`}, e.onServer(t, "Archive"), "copied, not lost")
	assert.Empty(t, e.onServer(t, "INBOX"))
	assert.Equal(t, []string{"done"}, e.opStates(t))
}

// A destination message whose Message-ID only looks alike to SEARCH, or of
// another size, is not the copy: the retry copies the message rather than
// expunge its only one.
func TestUncertainCopyMoveOtherSizeTwin(t *testing.T) {
	for name, twin := range map[string]string{
		"other size":    msg("m@x") + "a longer body\r\n",
		"other case":    msg("M@x"),  // SEARCH ignores case
		"longer, alike": msg("am@x"), // SEARCH matches substrings
	} {
		t.Run(name, func(t *testing.T) { uncertainCopyMoveWithTwin(t, twin) })
	}
}

func uncertainCopyMoveWithTwin(t *testing.T, twin string) {
	e := newSyncEnv(t, testmail.WithoutMove())
	e.put(t, "INBOX", msg("m@x"))
	e.put(t, "Archive", twin)
	e.sync(t)
	id := e.rows(t, "SELECT id FROM messages WHERE "+shownCol+" = ?", e.folderID(t, "INBOX"))[0][0].(int64)
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	_, err := e.s.db.Execute(bg, "UPDATE ops SET attempts = 1")
	require.NoError(t, err)
	c := e.dial(t)
	_, err = c.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	require.NoError(t, c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close())
	e.sync(t)
	e.sync(t)
	assert.Len(t, e.onServer(t, "Archive"), 2, "copied, not lost")
	assert.Empty(t, e.onServer(t, "INBOX"))
}

func TestStaleUIDNeverReplayed(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "Archive", msg("old@x"))
	e.sync(t)
	id := e.idOf(t, "old@x")
	e.im.Stop()
	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
	require.NoError(t, e.im.User.Delete("Archive"))
	require.NoError(t, e.im.User.Create("Archive", nil))
	e.put(t, "Archive", msg("new@x")) // UID 1 again
	e.im.Restart()
	ch, unsub := e.s.Subscribe()
	defer unsub()
	e.sync(t)

	assert.Equal(t, []string{"new@x"}, e.onServer(t, "Archive"))
	assert.Equal(t, []string{"conflict"}, e.opStates(t))
	ops := e.ops(t)
	require.Len(t, ops, 1)
	assert.NotEmpty(t, ops[0].LastError)
	assert.Len(t, ofType(collect(ch), events.SyncFailed), 1)
	assert.Equal(t, [][]any{{"new@x", ""}}, e.rows(t, "SELECT message_id, flags FROM messages"))
}

func TestRemoteFlagsDoNotOverwritePending(t *testing.T) {
	e := newSyncEnv(t)
	e.putFlagged(t, "INBOX", msg("m@x"), imap.FlagSeen)
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
	c := e.dial(t)
	_, err := c.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	require.NoError(t, c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close())
	require.NoError(t, c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close())

	_, ferr := mailsync.Fetch(bg, e.s.db, e.dial(t), e.acc.ID, e.s.bus, mailsync.FetchOptions{})
	require.NoError(t, ferr) // pull before push
	assert.Equal(t, `\Seen`, store.Str(e.rows(t, "SELECT flags FROM messages")[0][0]), "local change wins until pushed")

	e.sync(t)
	assert.Equal(t, []string{`m@x \Flagged \Seen`}, e.onServer(t, "INBOX"))
	assert.Equal(t, `\Flagged \Seen`, store.Str(e.rows(t, "SELECT flags FROM messages")[0][0]))
}

func TestFailedOpKeepsError(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	inbox := e.folderID(t, "INBOX")
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	require.NoError(t, e.im.User.Delete("Archive"))
	ch, unsub := e.s.Subscribe()
	defer unsub()
	for i := 1; i <= mailsync.MaxAttempts; i++ {
		e.sync(t)
		ops := e.ops(t)
		require.Len(t, ops, 1)
		assert.Equal(t, i, ops[0].Attempts)
		assert.NotEmpty(t, ops[0].LastError)
		failed := ofType(collect(ch), events.SyncFailed)
		if i < mailsync.MaxAttempts {
			assert.Equal(t, "pending", ops[0].State)
			assert.Empty(t, failed)
		} else {
			assert.Equal(t, "failed", ops[0].State)
			assert.Len(t, failed, 1)
		}
	}
	// The message never left INBOX on the server and is shown there again.
	assert.Equal(t, [][]any{{id, inbox}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "INBOX"))
}

func TestDeleteMovesToTrash(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	trash := e.folderID(t, "Trash")
	require.NoError(t, e.s.Delete(bg, []int64{id}))
	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, trash, m.FolderID)
	e.sync(t)
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Trash"))

	require.NoError(t, e.s.Delete(bg, []int64{id}))
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM ops"), "deleting from Trash queues nothing")
	e.sync(t)
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Trash"))
	assert.Equal(t, [][]any{{id, trash}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))
}

func TestOpsRejectBadInput(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	other, err := e.s.db.Execute(bg, "INSERT INTO accounts(email) VALUES ('bob@example.com')")
	require.NoError(t, err)
	res, err := e.s.db.Execute(bg, "INSERT INTO folders(account_id, name, role) VALUES (?, 'INBOX', 'inbox')", other.LastInsertID)
	require.NoError(t, err)
	res2, err := e.s.db.Execute(bg, "INSERT INTO messages(account_id, flags) VALUES (?, '')", other.LastInsertID)
	require.NoError(t, err)
	showIn(t, e.s, res2.LastInsertID, res.LastInsertID)

	assert.Error(t, e.s.SetFlags(bg, []int64{id, 999}, []string{`\Seen`}, nil), "unknown id")
	assert.Error(t, e.s.SetFlags(bg, []int64{id, res2.LastInsertID}, []string{`\Seen`}, nil), "mixed accounts")
	assert.Error(t, e.s.Move(bg, []int64{id}, res.LastInsertID), "folder of another account")
	assert.Error(t, e.s.Move(bg, []int64{id}, 999), "unknown folder")
	assert.Error(t, e.s.Delete(bg, []int64{id, res2.LastInsertID}), "mixed accounts")
	for _, bad := range []string{"", `\Deleted`, `\Recent`, "a b", "x)"} {
		assert.Error(t, e.s.SetFlags(bg, []int64{id}, []string{bad}, nil), bad)
	}
	assert.Zero(t, e.count(t, "SELECT count(*) FROM ops"))
	assert.Equal(t, "", store.Str(e.rows(t, "SELECT flags FROM messages WHERE id = ?", id)[0][0]))
	assert.Equal(t, e.folderID(t, "INBOX"), store.Int(e.rows(t, "SELECT "+shownCol+" FROM messages WHERE id = ?", id)[0][0]))
}

// A move queued after one that fails moves the message from where the server
// still has it, not from the failed move's destination.
func TestMoveAfterFailedMove(t *testing.T) {
	e := newSyncEnv(t)
	require.NoError(t, e.im.User.Create("Junk", nil))
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	junk := e.folderID(t, "Junk")
	e.im.Stop()
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	require.NoError(t, e.s.Move(bg, []int64{id}, junk))
	require.NoError(t, e.im.User.Delete("Archive"))
	e.im.Restart()
	for i := 0; i < mailsync.MaxAttempts; i++ {
		e.sync(t)
	}
	assert.Equal(t, []string{"failed", "done"}, e.opStates(t))
	assert.Empty(t, e.onServer(t, "INBOX"))
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Junk"))
	assert.Equal(t, [][]any{{id, junk}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))
}

func TestDependentRunsAfterFailedMove(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	e.im.Stop()
	require.NoError(t, e.s.Move(bg, []int64{id}, e.folderID(t, "Archive")))
	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
	require.NoError(t, e.im.User.Delete("Archive"))
	e.im.Restart()
	for i := 0; i < mailsync.MaxAttempts; i++ {
		e.sync(t)
	}
	assert.Equal(t, []string{"failed", "done"}, e.opStates(t))
	assert.Nil(t, e.rows(t, "SELECT depends_on FROM ops WHERE kind = 'flags'")[0][0], "re-based once its dependency failed")
	assert.Equal(t, []string{`m@x \Seen`}, e.onServer(t, "INBOX"))
	assert.Equal(t, [][]any{{id, e.folderID(t, "INBOX"), `\Seen`}}, e.rows(t, "SELECT id, "+shownCol+", flags FROM messages"))
}

func TestSetFlagsCaseInsensitive(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\SEEN`, `\flagged`, "$Label", "$label"}, nil))
	m, err := e.s.GetMessage(bg, id, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"$Label", `\Flagged`, `\Seen`}, m.Flags)
	e.sync(t)
	assert.Equal(t, []string{`m@x $label \Flagged \Seen`}, e.onServer(t, "INBOX")) // the memory server lower-cases keywords

	require.NoError(t, e.s.SetFlags(bg, []int64{id}, nil, []string{`\seen`, "$LABEL"}))
	m, _ = e.s.GetMessage(bg, id, false)
	assert.Equal(t, []string{`\Flagged`}, m.Flags)
	e.sync(t)
	assert.Equal(t, []string{`m@x \Flagged`}, e.onServer(t, "INBOX"))
}

func TestFetchKeepsConcurrentLocalFlags(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	c := e.dial(t)
	_, err := c.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	require.NoError(t, c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Close())
	// The user marks it read after fetch read the local flags but before it writes.
	mailsync.BeforeFlagsWrite = func() {
		mailsync.BeforeFlagsWrite = nil
		require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
	}
	t.Cleanup(func() { mailsync.BeforeFlagsWrite = nil })
	_, ferr := mailsync.Fetch(bg, e.s.db, e.dial(t), e.acc.ID, e.s.bus, mailsync.FetchOptions{})
	require.NoError(t, ferr)
	assert.Equal(t, `\Seen`, store.Str(e.rows(t, "SELECT flags FROM messages")[0][0]))
	e.sync(t)
	assert.Equal(t, `\Flagged \Seen`, store.Str(e.rows(t, "SELECT flags FROM messages")[0][0]))
	assert.Equal(t, []string{`m@x \Flagged \Seen`}, e.onServer(t, "INBOX"))
}

// A move the server reports without COPYUID, of a message without Message-ID,
// can never be claimed: it ends as a conflict instead of waiting forever, the
// stale row goes and the server copy is a message of its own.
func TestUnclaimableMoveEndsInConflict(t *testing.T) {
	e := newSyncEnv(t, testmail.WithoutCOPYUID())
	e.put(t, "INBOX", "From: bob@example.com\r\nTo: alice@example.com\r\nSubject: anon\r\nDate: "+day+"\r\n\r\nno id\r\n")
	e.sync(t)
	id := store.Int(e.rows(t, "SELECT id FROM messages WHERE subject = 'anon'")[0][0])
	archive := e.folderID(t, "Archive")
	require.NoError(t, e.s.Move(bg, []int64{id}, archive))
	require.NoError(t, e.s.SetFlags(bg, []int64{id}, []string{`\Seen`}, nil))
	ch, unsub := e.s.Subscribe()
	defer unsub()
	e.sync(t)

	assert.NotEmpty(t, ofType(collect(ch), events.SyncFailed))
	assert.Equal(t, []string{"conflict", "conflict"}, e.opStates(t))
	assert.Zero(t, e.count(t, "SELECT count(*) FROM messages WHERE id = ?", id))
	r := e.rows(t, "SELECT "+shownCol+", "+serverCol+" FROM messages WHERE subject = 'anon'")
	require.Len(t, r, 1)
	assert.Equal(t, []any{archive, archive}, r[0])
	assert.Equal(t, []string{""}, e.onServer(t, "Archive"), "moved, and the flag change was not applied")
	assert.Empty(t, e.onServer(t, "INBOX"))
	e.sync(t)
	assert.Equal(t, []string{"conflict", "conflict"}, e.opStates(t))
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE subject = 'anon'"))
}

func TestMoveToJunk(t *testing.T) {
	e := newSyncEnv(t)
	require.NoError(t, e.im.User.Create("Junk", nil))
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.MoveToRole(bg, []int64{id}, "junk"))
	assert.Equal(t, [][]any{{id, e.folderID(t, "Junk")}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))
	e.sync(t)
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "Junk"))
	assert.Equal(t, []string{}, e.onServer(t, "INBOX"))
}

func TestMoveToInbox(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "Trash", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	require.NoError(t, e.s.MoveToRole(bg, []int64{id}, "inbox"))
	assert.Equal(t, [][]any{{id, e.folderID(t, "INBOX")}}, e.rows(t, "SELECT id, "+shownCol+" FROM messages"))
	e.sync(t)
	assert.Equal(t, []string{"m@x"}, e.onServer(t, "INBOX"))
	assert.Equal(t, []string{}, e.onServer(t, "Trash"))
}

func TestMoveToRoleWithoutFolder(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("m@x"))
	e.sync(t)
	id := e.idOf(t, "m@x")
	err := e.s.MoveToRole(bg, []int64{id}, "junk")
	var nf *ErrNoRoleFolder
	require.ErrorAs(t, err, &nf)
	assert.Equal(t, "junk", nf.Role)
	assert.Error(t, e.s.MoveToRole(bg, []int64{id}, "drafts"), "only inbox, trash, junk and archive")
	assert.Zero(t, e.count(t, "SELECT count(*) FROM ops"))
	assert.Equal(t, e.folderID(t, "INBOX"), store.Int(e.rows(t, "SELECT "+shownCol+" FROM messages WHERE id = ?", id)[0][0]))
}
