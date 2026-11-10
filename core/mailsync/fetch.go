// Package mailsync pulls folders and messages from IMAP into the store.
package mailsync

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
)

// One round of body-text FETCHes covers at most textBatch messages and asks for
// at most textBatchBytes (or one message's text), which bounds sync memory.
const (
	textBatch      = 50
	textBatchBytes = 32 << 20
)

// PartFetchBytes is how much of one text part a sync downloads: enough encoded
// bytes for mail.MaxBodyBytes of text even in quoted-printable. Tests lower it.
var PartFetchBytes int64 = 3 * mail.MaxBodyBytes

// BackfillPage is how many messages one FETCH of a range backfill asks for,
// which bounds its memory. Tests lower it.
var BackfillPage = 1000

// BackfillPages is how many pages a range backfill takes per sync round
// before it yields, so new mail, ops and SyncNow wait at most that long; the
// next round goes on. Tests lower it.
var BackfillPages = 2

// account is a sync's view of the account: limit > 0 keeps the newest limit
// messages per folder, else months > 0 the mail since the cutoff, else all.
type account struct {
	id     int64
	email  string
	limit  int
	months int
	budget int  // range backfill pages left this round
	noSelf bool // a local archive: email is a placeholder, not the user's address
}

// since is the SINCE date of a months range: date-only, as IMAP compares
// INTERNALDATE by day; zero for all mail.
func (a *account) since(now time.Time) time.Time {
	if a.months <= 0 {
		return time.Time{}
	}
	y, m, d := now.AddDate(0, -a.months, 0).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// result collects what one Fetch changed, for the events it publishes.
type result struct {
	received, threads, changed, changedThreads []int64
	orphans                                    []int64 // Gmail messages that lost a server location, for the round's end
	expunged                                   []int64 // of them, gone from Trash or Spam: Gmail deleted them for good
	conflicts                                  []string
	more                                       bool // a range backfill yielded with work left
}

// gone records messages about to be deleted (or moved between threads) as changed.
func (r *result) gone(ids, tids []int64) {
	r.changed = append(r.changed, ids...)
	r.changedThreads = append(r.changedThreads, tids...)
}

// merged repoints thread IDs collected so far from the merged-away threads to target.
func (r *result) merged(target int64, others []int64) {
	for _, l := range [][]int64{r.threads, r.changedThreads} {
		for i, t := range l {
			if slices.Contains(others, t) {
				l[i] = target
			}
		}
	}
	r.changedThreads = append(r.changedThreads, target)
}

// FetchOptions narrow one round. The zero value syncs every folder.
type FetchOptions struct {
	// InboxOnly syncs the inbox and no other folder: a background round,
	// where only what arrives matters, still gets new mail for its
	// notification while the other folders wait for a full round. The round
	// ends after the inbox, so the passes over every folder are skipped.
	InboxOnly bool
}

// Fetch syncs the account's folders over c: folder list, new messages
// (headers and text parts only; attachments stay lazy), threading, remote flag
// changes and expunges. Rows committed before an error are still announced.
// more reports that a range backfill used its pages for the round and has
// work left: the caller should sync again soon.
func Fetch(ctx context.Context, db *store.DB, c *imapclient.Client, accountID int64, bus *events.Bus, opts FetchOptions) (more bool, err error) {
	var r result
	complete := false
	defer func() {
		if complete {
			err = errors.Join(err, r.logArrivals(context.WithoutCancel(ctx), db, accountID))
		}
		r.publish(bus, accountID)
	}()
	a, err := loadAccount(ctx, db, accountID)
	if err != nil {
		return false, err
	}
	a.budget = BackfillPages
	folders, err := syncFolders(ctx, db, c, accountID, &r)
	if err != nil {
		return false, err
	}
	gmail := c.Caps().Has(capGmail)
	if gmail && !opts.InboxOnly {
		if err := mergeOld(ctx, db, c, a, folders, &r); err != nil {
			return false, err
		}
	}
	if opts.InboxOnly {
		folders = slices.DeleteFunc(folders, func(f *folder) bool { return f.role != "inbox" })
	}
	// A folder the server refuses is skipped, so it cannot hold back the
	// others' arrivals round after round; a broken connection still stops here.
	var refused error
	for _, f := range folders {
		if err := syncFolder(ctx, db, c, a, f, &r); err != nil {
			err = fmt.Errorf("sync %q: %w", f.name, err)
			var no *imap.Error
			if !errors.As(err, &no) || ctx.Err() != nil {
				return r.more, err
			}
			refused = errors.Join(refused, err)
		}
	}
	complete = true
	if refused != nil { // a skipped folder may yet hold a membership, a claim or a stale location
		return r.more, refused
	}
	if opts.InboxOnly {
		// The passes below decide over every folder — locations, orphans,
		// claims — and a round that read one folder cannot; a full round does.
		return r.more, nil
	}
	if gmail {
		if err := keepInAllMail(ctx, db, c, accountID, &r); err != nil {
			return r.more, err
		}
	}
	if err := dropOrphans(ctx, db, accountID, r.orphans, true, gmail, &r); err != nil {
		return r.more, err
	}
	if gmail {
		if err := reshow(ctx, db, accountID, &r); err != nil {
			return r.more, err
		}
	}
	return r.more, unclaimable(ctx, db, accountID, &r)
}

// keepInAllMail gives each Gmail orphan dropOrphans would delete its All Mail
// UID when All Mail still has it: a count-limited sync may never have cached
// that UID, so a complete scan alone does not prove the message is gone.
func keepInAllMail(ctx context.Context, db *store.DB, c *imapclient.Client, accountID int64, r *result) error {
	all, err := db.Query(ctx, "SELECT id, name FROM folders WHERE account_id = ? AND role = 'all'", accountID)
	if err != nil || len(all.Values) == 0 {
		return err
	}
	rows, err := db.Query(ctx, `SELECT id, gmail_msgid, thread_id FROM messages WHERE account_id = ? AND gmail_msgid IS NOT NULL AND `+orphaned+`
		AND NOT EXISTS (SELECT 1 FROM ops WHERE message_id = messages.id AND state = 'pending')`, accountID)
	if err != nil || len(rows.Values) == 0 {
		return err
	}
	folder := store.Int(all.Values[0][0])
	sel, err := c.Select(store.Str(all.Values[0][1]), nil).Wait()
	if err != nil {
		return err
	}
	var stmts []sqlite.Statement
	var ids, tids []int64
	for _, v := range rows.Values {
		gid, _ := strconv.ParseUint(store.Str(v[1]), 10, 64)
		found, err := c.UIDSearch(&imap.SearchCriteria{GmailMsgID: []uint64{gid}}, nil).Wait()
		if err != nil {
			return err
		}
		if uid := single(found.All); uid != 0 {
			stmts = append(stmts, locate(store.Int(v[0]), folder, sel.UIDValidity, uid)...)
			ids, tids = append(ids, store.Int(v[0])), append(tids, store.Int(v[2]))
		}
	}
	if len(stmts) == 0 {
		return nil
	}
	if _, err := db.Transaction(ctx, stmts); err != nil {
		return err
	}
	r.gone(ids, tids)
	return nil
}

// reshow shows a Gmail message again in every folder the server has it in
// once no move or purge of it waits: a location found while one was pending
// was added hidden, and nothing else shows it after the move succeeds.
func reshow(ctx context.Context, db *store.DB, accountID int64, r *result) error {
	const hidden = `FROM message_locations l JOIN messages ON messages.id = l.message_id
		WHERE messages.account_id = ? AND l.uid IS NOT NULL AND NOT l.shown AND ` + notMoving
	rows, err := db.Query(ctx, "SELECT DISTINCT messages.id, messages.thread_id "+hidden, accountID)
	if err != nil || len(rows.Values) == 0 {
		return err
	}
	if _, err := db.Execute(ctx, `UPDATE message_locations SET shown = 1, entered_at = unixepoch() WHERE rowid IN (SELECT l.rowid `+hidden+`)`, accountID); err != nil {
		return err
	}
	var ids, tids []int64
	for _, v := range rows.Values {
		ids, tids = append(ids, store.Int(v[0])), append(tids, store.Int(v[1]))
	}
	r.gone(ids, tids)
	return nil
}

// publish announces what r collected.
func (r *result) publish(bus *events.Bus, accountID int64) {
	if len(r.received) > 0 {
		bus.Publish(events.Event{Type: events.MessageReceived, AccountID: accountID, MessageIDs: r.received, ThreadIDs: r.threads})
	}
	if len(r.changed) > 0 {
		bus.Publish(events.Event{Type: events.MessagesChanged, AccountID: accountID, MessageIDs: slices.Compact(slices.Sorted(slices.Values(r.changed))),
			ThreadIDs: slices.Compact(slices.Sorted(slices.Values(r.changedThreads)))})
	}
	if len(r.conflicts) > 0 {
		bus.Publish(events.Event{Type: events.SyncFailed, AccountID: accountID, Error: strings.Join(r.conflicts, "; ")})
	}
}

func loadAccount(ctx context.Context, db *store.DB, accountID int64) (*account, error) {
	rows, err := db.Query(ctx, "SELECT email, sync_limit, sync_since_months FROM accounts WHERE id = ?", accountID)
	if err != nil {
		return nil, err
	}
	if len(rows.Values) == 0 {
		return nil, fmt.Errorf("account %d not found", accountID)
	}
	return &account{id: accountID, email: store.Str(rows.Values[0][0]), limit: int(store.Int(rows.Values[0][1])),
		months: int(store.Int(rows.Values[0][2]))}, nil
}

func syncFolder(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, r *result) error {
	sel, err := c.Select(f.name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return err
	}
	if f.uidValidity != sel.UIDValidity {
		if err := resetFolder(ctx, db, c, a.id, f, sel.UIDValidity, r); err != nil {
			return err
		}
	}
	if err := fetchNew(ctx, db, c, a, f, sel.NumMessages, r); err != nil {
		return err
	}
	if err := backfill(ctx, db, c, a, f, sel.NumMessages, r); err != nil {
		return err
	}
	if err := refreshAuthenticationResults(ctx, db, c, a, f, r); err != nil {
		return err
	}
	return syncFlags(ctx, db, c, a.id, f, r)
}

// resetFolder drops everything known about the folder's old UIDs: their
// locations, and the messages the server then holds nowhere (orphan), and
// pending ops that would act on them become conflicts: those taking a message
// from the folder, and every one of a message held nowhere else.
func resetFolder(ctx context.Context, db *store.DB, c *imapclient.Client, accountID int64, f *folder, uidValidity uint32, r *result) error {
	ids, _, err := doomed(ctx, db, serverIn, f.id)
	if err != nil {
		return err
	}
	stmts := []sqlite.Statement{
		{SQL: `UPDATE ops SET state = 'conflict' WHERE state = 'pending' AND account_id = ?
			AND message_id IN (SELECT id FROM messages WHERE ` + serverIn + `) AND (src_folder_id = ?
				OR NOT EXISTS (SELECT 1 FROM message_locations WHERE message_id = ops.message_id AND folder_id != ? AND uid IS NOT NULL))`,
			Args: []any{accountID, f.id, f.id, f.id}},
		{SQL: "DELETE FROM message_locations WHERE folder_id = ? AND uid IS NOT NULL", Args: []any{f.id}},
		{SQL: "UPDATE folders SET uidvalidity = ?, last_uid = 0, synced = 0, filled_since = NULL WHERE id = ?", Args: []any{uidValidity, f.id}},
	}
	if _, err := db.Transaction(ctx, stmts); err != nil {
		return err
	}
	f.uidValidity, f.lastUID, f.synced, f.filled = uidValidity, 0, false, nil
	return r.orphan(ctx, db, c, accountID, ids)
}

// orphaned is the condition on messages (unaliased) that the server holds the
// message nowhere Godwit knows and no move of it awaits its claim: it is gone.
const orphaned = unlocated + " AND NOT EXISTS (SELECT 1 FROM ops WHERE message_id = messages.id AND state = 'awaiting_claim')"

// orphan deletes those of ids, messages sync just dropped a server location
// of, that are orphaned. Gmail's wait for the round's end (Fetch): a folder
// synced later may hold them still (ADR 0034).
func (r *result) orphan(ctx context.Context, db *store.DB, c *imapclient.Client, accountID int64, ids []int64) error {
	if c.Caps().Has(capGmail) {
		r.orphans = append(r.orphans, ids...)
		return nil
	}
	return dropOrphans(ctx, db, accountID, ids, false, false, r)
}

// dropOrphans deletes the orphaned messages of ids (attachments and FTS rows go
// with them), and with stray those of the account without a location left,
// should a round that dropped their last one have stopped before its end.
// Cached attachment files are removed best effort after the commit; one that
// cannot be removed is no longer referenced, so the startup orphan sweep of
// the cache directory (store.CleanOrphans) collects it.
func dropOrphans(ctx context.Context, db *store.DB, accountID int64, ids []int64, stray, gmail bool, r *result) error {
	which := "id IN (" + marks(len(ids)) + ")"
	if stray {
		which = "(" + which + " OR NOT EXISTS (SELECT 1 FROM message_locations WHERE message_id = messages.id))"
	}
	if len(ids) == 0 && !stray {
		return nil
	}
	where, args := "account_id = ? AND "+which+" AND "+orphaned, append([]any{accountID}, anys(ids)...)
	if gmail {
		where += ` AND NOT EXISTS (SELECT 1 FROM ops WHERE message_id = messages.id AND state = 'pending')
			AND (gmail_msgid IS NULL OR id IN (` + marks(len(r.expunged)) + `)
				OR EXISTS (SELECT 1 FROM folders WHERE account_id = messages.account_id AND role = 'all'))`
		args = append(args, anys(r.expunged)...)
	}
	gone, tids, err := doomed(ctx, db, where, args...)
	if err != nil || len(gone) == 0 {
		return err
	}
	files, err := db.Query(ctx, "SELECT cache_path FROM attachments WHERE cache_path IS NOT NULL AND message_id IN ("+marks(len(gone))+")", anys(gone)...)
	if err != nil {
		return err
	}
	stmts := []sqlite.Statement{{SQL: "DELETE FROM messages WHERE id IN (" + marks(len(gone)) + ")", Args: anys(gone)}}
	if _, err := db.Transaction(ctx, append(stmts, tidyThreads(accountID, tids)...)); err != nil {
		return err
	}
	r.gone(gone, tids)
	for _, p := range files.Values {
		os.Remove(store.Str(p[0])) // best effort, see above
	}
	return nil
}

// doomed returns the IDs and thread IDs of the messages matching where.
func doomed(ctx context.Context, db *store.DB, where string, args ...any) (ids, tids []int64, err error) {
	rows, err := db.Query(ctx, "SELECT id, thread_id FROM messages WHERE "+where, args...)
	if err != nil {
		return nil, nil, err
	}
	for _, v := range rows.Values {
		ids = append(ids, store.Int(v[0]))
		if t := store.Int(v[1]); t != 0 {
			tids = append(tids, t)
		}
	}
	return ids, tids, nil
}

func headerSection() *imap.FetchItemBodySection {
	return &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader,
		HeaderFields: []string{"MESSAGE-ID", "DATE", "SUBJECT", "FROM", "TO", "CC", "REFERENCES", "IN-REPLY-TO", "REPLY-TO", "LIST-UNSUBSCRIBE", "LIST-UNSUBSCRIBE-POST", "LIST-ID", "AUTO-SUBMITTED", "PRECEDENCE", "AUTHENTICATION-RESULTS", "DKIM-SIGNATURE", "RECEIVED", "MIME-VERSION", "CONTENT-TYPE", "X-GODWIT-RULE"}, Peek: true}
}

// fetchNew imports messages above last_uid; the first sync of a count range
// takes only the newest sync_limit (n is the mailbox's message count), and that
// of a months or all range only notes the top UID, leaving the mail to the
// backfill. What it imports into a folder whose first sync completed is new
// mail, whatever its date (a folder synced empty included); completing one
// marks it.
func fetchNew(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, n uint32, r *result) error {
	if n == 0 {
		return advance(ctx, db, f, f.lastUID)
	}
	if !f.synced && a.limit <= 0 { // a folder synced empty gets its mail as new below
		top, err := c.Fetch(imap.SeqSetNum(n), &imap.FetchOptions{UID: true}).Collect()
		if err != nil {
			return err
		}
		if len(top) == 0 {
			return fmt.Errorf("no message %d", n)
		}
		return advance(ctx, db, f, uint32(top[0].UID))
	}
	// A first sync, or the reimport after a UIDVALIDITY reset (which clears
	// synced), is not new mail, even if an earlier attempt stopped partway.
	arrivals := f.synced
	if f.lastUID == 0 && a.limit > 0 {
		start := uint32(1)
		if n > uint32(a.limit) {
			start = n - uint32(a.limit) + 1
		}
		var s imap.SeqSet
		s.AddRange(start, 0)
		last, err := importAbove(ctx, db, c, a, f, s, arrivals, nil, r)
		if err != nil {
			return err
		}
		return advance(ctx, db, f, last)
	}
	// A page at a time: a folder that gains 50,000 messages at once (a bulk
	// move in webmail) never sits in memory whole, and a dropped connection
	// keeps the pages already imported.
	var above imap.UIDSet
	above.AddRange(imap.UID(f.lastUID+1), 0)
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{above}}, nil).Wait()
	if err != nil {
		return err
	}
	uids := slices.DeleteFunc(found.AllUIDs(), func(u imap.UID) bool { return uint32(u) <= f.lastUID }) // "n:*" includes n when nothing is new
	slices.Sort(uids)
	var twins map[string]int // nil: a single page counts its own
	if len(uids) > BackfillPage {
		if twins, err = arrivalTwins(ctx, db, c, a, f, uids); err != nil {
			return err
		}
	}
	for page := range slices.Chunk(uids, BackfillPage) {
		last, err := importAbove(ctx, db, c, a, f, imap.UIDSetNum(page...), arrivals, twins, r)
		if err != nil {
			return err
		}
		if _, err := db.Execute(ctx, "UPDATE folders SET last_uid = ? WHERE id = ?", last, f.id); err != nil {
			return err
		}
		f.lastUID = last
	}
	return advance(ctx, db, f, f.lastUID)
}

// arrivalTwins counts uids' arrivals per Message-ID and size when a move into f
// awaits its claim: a claim needs that count across every page, so two copies
// on different pages are not each taken for the only one. Nil when none waits.
func arrivalTwins(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, uids []imap.UID) (map[string]int, error) {
	w, err := db.Query(ctx, "SELECT 1 FROM ops WHERE account_id = ? AND state = 'awaiting_claim' AND dst_folder_id = ? LIMIT 1", a.id, f.id)
	if err != nil || len(w.Values) == 0 {
		return nil, err
	}
	msgs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{headerSection()}}).Collect()
	if err != nil {
		return nil, err
	}
	twins := map[string]int{}
	for _, m := range msgs {
		twins[twinKey(m)]++
	}
	return twins, nil
}

// importAbove imports the messages of set above last_uid that are not stored
// yet, in UID order, and returns the highest UID set has above last_uid.
func importAbove(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, set imap.NumSet, arrivals bool, twins map[string]int, r *result) (uint32, error) {
	msgs, top, err := fetchImport(ctx, db, c, a, f, set, arrivals, r)
	if err != nil {
		return 0, err
	}
	have, err := db.Query(ctx, "SELECT uid FROM message_locations WHERE folder_id = ? AND uid > ?", f.id, f.lastUID)
	if err != nil {
		return 0, err
	}
	known := map[uint32]bool{}
	for _, v := range have.Values {
		known[uint32(store.Int(v[0]))] = true
	}
	last, todo := max(f.lastUID, top), msgs[:0]
	for _, m := range msgs {
		if uid := uint32(m.UID); uid > f.lastUID && !known[uid] {
			todo = append(todo, m)
		}
	}
	slices.SortFunc(todo, byUID)
	return last, importAll(ctx, db, c, a, f, todo, arrivals, twins, r)
}

// advance records a completed fetchNew: last_uid and synced in one write.
func advance(ctx context.Context, db *store.DB, f *folder, last uint32) error {
	if last == f.lastUID && f.synced {
		return nil
	}
	if _, err := db.Execute(ctx, "UPDATE folders SET last_uid = ?, synced = 1 WHERE id = ?", last, f.id); err != nil {
		return err
	}
	f.lastUID, f.synced = last, true
	return nil
}

// capGmail marks a Gmail server, which answers X-GM-MSGID (ADR 0034).
const capGmail imap.Cap = "X-GM-EXT-1"

// importItems is what a sync fetches of each message before its text parts,
// with X-GM-MSGID only from a Gmail server: others refuse the item.
func importItems(c *imapclient.Client) *imap.FetchOptions {
	return &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, InternalDate: true, GmailMsgID: c.Caps().Has(capGmail),
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true}, BodySection: []*imap.FetchItemBodySection{headerSection()}}
}

// gmailMsgID is m's X-GM-MSGID as stored: decimal text, NULL when not fetched.
func gmailMsgID(m *imapclient.FetchMessageBuffer) any {
	if m.GmailMsgID == 0 {
		return nil
	}
	return strconv.FormatUint(m.GmailMsgID, 10)
}

// importAll imports msgs in the order given, fetching their text parts in
// batches; with arrivals set each inserted message logs a message.received event.
// twins counts arrivals per Message-ID and size, for claims; nil counts msgs.
func importAll(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, msgs []*imapclient.FetchMessageBuffer, arrivals bool, twins map[string]int, r *result) error {
	if twins == nil {
		twins = map[string]int{}
		for _, m := range msgs {
			twins[twinKey(m)]++
		}
	}
	if err := fillStructures(c, msgs); err != nil {
		return err
	}
	for _, batch := range textBatches(msgs) {
		texts, err := fetchTexts(c, batch)
		if err != nil {
			return err
		}
		for _, m := range batch {
			if err := importMessage(ctx, db, a, f, m, texts[m.UID], twins[twinKey(m)], arrivals, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// fillStructures gives each of msgs whose body structure the server sent
// malformed (Gmail, #389; go-imap leaves it nil) one built from the whole
// message, downloaded once, so its text and attachments still import.
func fillStructures(c *imapclient.Client, msgs []*imapclient.FetchMessageBuffer) error {
	whole := &imap.FetchItemBodySection{Peek: true}
	for _, m := range msgs {
		if m.BodyStructure != nil {
			continue
		}
		bufs, err := c.Fetch(imap.UIDSetNum(m.UID), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{whole}}).Collect()
		if err != nil {
			return err
		}
		if len(bufs) == 1 { // none: expunged meanwhile, it imports without parts
			m.BodyStructure = imapserver.ExtractBodyStructure(bytes.NewReader(bufs[0].FindBodySection(whole)))
		}
	}
	return nil
}

// backfill imports older messages a lower sync_limit left out: while the folder
// holds fewer than sync_limit messages, the newest server UIDs below the oldest
// one stored, up to the shortfall (n is the mailbox's message count). Stored
// messages and pending ops are untouched; a narrower range never deletes mail.
// A count pages only below the oldest stored UID: after a months range left
// gaps above it (old-dated mail with high UIDs), those stay unfilled, which
// LoadOlder and 全部 still reach.
func backfill(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, n uint32, r *result) error {
	if f.lastUID == 0 {
		return nil
	}
	if a.limit <= 0 {
		return backfillRange(ctx, db, c, a, f, r)
	}
	rows, err := db.Query(ctx, "SELECT count(*), min(uid) FROM message_locations WHERE folder_id = ? AND uid IS NOT NULL", f.id)
	if err != nil {
		return err
	}
	have, below := int(store.Int(rows.Values[0][0])), f.lastUID+1
	if have > 0 {
		below = uint32(store.Int(rows.Values[0][1]))
	}
	if have >= a.limit || have >= int(n) || below <= 1 {
		return nil
	}
	var older imap.UIDSet
	older.AddRange(1, imap.UID(below-1))
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{older}}, nil).Wait()
	if err != nil {
		return err
	}
	uids := found.AllUIDs()
	slices.Sort(uids)
	uids = uids[max(len(uids)-(a.limit-have), 0):]
	if len(uids) == 0 {
		return nil
	}
	msgs, _, err := fetchImport(ctx, db, c, a, f, imap.UIDSetNum(uids...), false, r)
	if err != nil {
		return err
	}
	// Newest first: if this stops partway, the oldest stored UID still bounds
	// what is missing and the next round resumes below it.
	slices.SortFunc(msgs, func(x, y *imapclient.FetchMessageBuffer) int { return byUID(y, x) })
	return importAll(ctx, db, c, a, f, msgs, false, nil, r) // older mail, not new arrivals
}

// backfillRange imports the messages up to last_uid that a months range (or
// all) covers and the folder lacks: UID SEARCH SINCE the cutoff, less what is
// stored, newest first in pages. UIDs and INTERNALDATE are not monotonic, so it
// diffs the whole window rather than paging below the oldest stored UID. A
// window filled once is not searched again until it grows (filled_since).
func backfillRange(ctx context.Context, db *store.DB, c *imapclient.Client, a *account, f *folder, r *result) error {
	since := a.since(time.Now())
	mark := int64(0)
	if !since.IsZero() {
		mark = since.Unix()
	}
	if f.filled != nil && *f.filled <= mark {
		return nil
	}
	if a.budget <= 0 {
		r.more = true // this round's pages are spent
		return nil
	}
	var upto imap.UIDSet
	upto.AddRange(1, imap.UID(f.lastUID))
	found, err := c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{upto}, Since: since}, nil).Wait()
	if err != nil {
		return err
	}
	have, err := db.Query(ctx, "SELECT uid FROM message_locations WHERE folder_id = ? AND uid IS NOT NULL", f.id)
	if err != nil {
		return err
	}
	known := map[imap.UID]bool{}
	for _, v := range have.Values {
		known[imap.UID(store.Int(v[0]))] = true
	}
	uids := slices.DeleteFunc(found.AllUIDs(), func(u imap.UID) bool { return known[u] })
	slices.SortFunc(uids, func(x, y imap.UID) int { return cmp.Compare(y, x) })
	for page := range slices.Chunk(uids, BackfillPage) {
		if a.budget <= 0 {
			r.more = true // filled_since stays unset: the next round searches again
			return nil
		}
		a.budget--
		msgs, _, err := fetchImport(ctx, db, c, a, f, imap.UIDSetNum(page...), false, r)
		if err != nil {
			return err
		}
		slices.SortFunc(msgs, func(x, y *imapclient.FetchMessageBuffer) int { return byUID(y, x) })
		if err := importAll(ctx, db, c, a, f, msgs, false, nil, r); err != nil { // older mail, not new arrivals
			return err
		}
	}
	if _, err := db.Execute(ctx, "UPDATE folders SET filled_since = ? WHERE id = ?", mark, f.id); err != nil {
		return err
	}
	f.filled = &mark
	return nil
}

func byUID(x, y *imapclient.FetchMessageBuffer) int { return cmp.Compare(x.UID, y.UID) }

func twinKey(m *imapclient.FetchMessageBuffer) string {
	h, err := mail.Parse(m.FindBodySection(headerSection()))
	if err != nil {
		return ""
	}
	return h.MessageID + "\x00" + strconv.FormatInt(m.RFC822Size, 10)
}

// textParts returns the text parts of m that a sync downloads: in order, until
// the message used enough for MaxBodyBytes of both text and HTML. Each is
// fetched up to PartFetchBytes; toParsed marks what was left out as truncated.
func textParts(m *imapclient.FetchMessageBuffer) []mail.Attachment {
	parts, _ := mail.ParseStructure(m.BodyStructure)
	budget := 2 * PartFetchBytes
	for i, p := range parts {
		if budget <= 0 {
			return parts[:i]
		}
		budget -= partCost(p)
	}
	return parts
}

// partCost is how many bytes fetching the text part may return.
func partCost(p mail.Attachment) int64 {
	if p.Size > 0 && int64(p.Size) < PartFetchBytes {
		return int64(p.Size)
	}
	return PartFetchBytes
}

// textBatches splits msgs into rounds of at most textBatch messages and
// textBatchBytes of requested text; a message never splits.
func textBatches(msgs []*imapclient.FetchMessageBuffer) [][]*imapclient.FetchMessageBuffer {
	var out [][]*imapclient.FetchMessageBuffer
	var cur []*imapclient.FetchMessageBuffer
	var size int64
	for _, m := range msgs {
		var cost int64
		for _, p := range textParts(m) {
			cost += partCost(p)
		}
		if len(cur) > 0 && (len(cur) == textBatch || size+cost > textBatchBytes) {
			out, cur, size = append(out, cur), nil, 0
		}
		cur, size = append(cur, m), size+cost
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// fetchTexts downloads the first PartFetchBytes of each of textParts of msgs,
// keyed by UID then part path. Messages with the same parts share one UID FETCH.
func fetchTexts(c *imapclient.Client, msgs []*imapclient.FetchMessageBuffer) (map[imap.UID]map[string][]byte, error) {
	groups := map[string][]imap.UID{}
	layout := map[string][]mail.Attachment{}
	for _, m := range msgs {
		parts := textParts(m)
		if len(parts) == 0 {
			continue
		}
		var key []string
		for _, p := range parts {
			key = append(key, p.Part)
		}
		k := strings.Join(key, " ")
		groups[k] = append(groups[k], m.UID)
		layout[k] = parts
	}
	out := map[imap.UID]map[string][]byte{}
	for k, uids := range groups {
		var secs []*imap.FetchItemBodySection
		for _, p := range layout[k] {
			secs = append(secs, &imap.FetchItemBodySection{Part: partPath(p.Part), Peek: true,
				Partial: &imap.SectionPartial{Size: PartFetchBytes}})
		}
		bufs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodySection: secs}).Collect()
		if err != nil {
			return nil, err
		}
		for _, b := range bufs {
			got := map[string][]byte{}
			for i, s := range secs {
				got[layout[k][i].Part] = b.FindBodySection(s)
			}
			out[b.UID] = got
		}
	}
	return out, nil
}

func partPath(p string) []int {
	var out []int
	for _, s := range strings.Split(p, ".") {
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}

// toParsed assembles a message from the fetched header fields and the decoded
// text parts. It reads no ENVELOPE: go-imap fails the whole FETCH on a server's
// malformed one, so one bad message would stop the folder's sync.
func toParsed(m *imapclient.FetchMessageBuffer, texts map[string][]byte) *mail.Parsed {
	p := &mail.Parsed{}
	if h, err := mail.Parse(m.FindBodySection(headerSection())); err == nil {
		p.MessageID, p.Subject, p.Date, p.From, p.To, p.Cc = h.MessageID, h.Subject, h.Date, h.From, h.To, h.Cc
		p.InReplyTo, p.References, p.ReplyTo = h.InReplyTo, h.References, h.ReplyTo
		p.ListUnsubscribe, p.ListUnsubscribePost, p.ListID = h.ListUnsubscribe, h.ListUnsubscribePost, h.ListID
		p.AutoSubmitted, p.Precedence = h.AutoSubmitted, h.Precedence
		p.AuthenticationResults, p.AuthenticationTrace, p.DKIMSigned = h.AuthenticationResults, h.AuthenticationTrace, h.DKIMSigned
		p.RuleSent = h.RuleSent
	}
	if p.Date.IsZero() {
		p.Date = m.InternalDate
	}
	parts, atts := mail.ParseStructure(m.BodyStructure)
	p.Attachments = atts
	for _, tp := range parts {
		raw, fetched := texts[tp.Part]
		if cut := int64(tp.Size) > PartFetchBytes; cut || !fetched {
			p.Truncated = true
			if i := bytes.LastIndexByte(raw, '\n'); cut && i >= 0 {
				raw = raw[:i+1] // whole lines decode cleanly in base64 and quoted-printable
			}
		}
		b, err := mail.DecodePart(raw, tp.Encoding, tp.Charset)
		if err != nil {
			b = raw // undecodable: keep the raw text rather than nothing
		}
		dst, html := &p.Text, tp.ContentType == "text/html"
		if html {
			dst = &p.HTML
		}
		b = mail.ValidText(b, tp.Charset, html) // a mislabeled charset must not fail the folder's sync in SQLite
		p.Truncated = appendCapped(dst, b) || p.Truncated
	}
	if p.Text == "" && p.HTML != "" {
		p.Text = mail.HTMLToText(p.HTML)
	}
	return p
}

// appendCapped appends b to *dst, cutting at a rune boundary so *dst stays within
// MaxBodyBytes. It reports whether anything was cut.
func appendCapped(dst *string, b []byte) bool {
	room := mail.MaxBodyBytes - len(*dst)
	if len(b) <= room {
		*dst += string(b)
		return false
	}
	cut := max(room, 0)
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	*dst += string(b[:cut])
	return true
}

func flagString(fl []imap.Flag) string {
	var out []string
	for _, f := range fl {
		if f != "\\Recent" {
			out = append(out, string(f))
		}
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

func jsonList(l []string) string {
	b, _ := json.Marshal(append([]string{}, l...))
	return string(b)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
