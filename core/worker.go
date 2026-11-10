package core

import (
	"context"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/godwit-email/godwit/core/conn"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/mailsync"
	"github.com/godwit-email/godwit/core/store"
)

// Background sync timing; tests lower the backoff.
var (
	idleMax      = 5 * time.Minute // longest IDLE on INBOX before a sync of every folder
	pollInterval = 5 * time.Minute // wait between syncs when the server lacks IDLE
	backoffMin   = 2 * time.Second // first wait after a failure, doubled per failure
	backoffMax   = 5 * time.Minute
	// chatterWithin: a server update that ends IDLE sooner than this after it
	// began is chatter. A run of chatter waits between rounds as failures do, so a
	// server that keeps reporting changes (or one the sync itself sets off) cannot
	// spin the worker through back-to-back full syncs (#453).
	chatterWithin = 10 * time.Second
)

// Test hooks, nil in production: beforeWorkerExit runs as a worker exits (tests
// make exits slow), onParked each time a worker parks on reauth_required.
var (
	beforeWorkerExit func()
	onParked         func(id int64)
)

// wakeSource is what a time set with wakeWorkerAt is for. Each source keeps its
// own time, so one does not drop another's.
type wakeSource int

const (
	wakeOutbox wakeSource = iota // the next queued outbox item comes due
	wakeOps                      // the next purge or archive undo window ends
	wakeSources
)

// worker is one account's background sync goroutine.
type worker struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{} // buffered 1: a local change asks for a sync
	// due is when each source wants a sync, zero for none; stopTimer stops the
	// timer set for the earliest, nil if none. Under workersMu.
	due       [wakeSources]time.Time
	stopTimer func() bool
}

// startWorker starts the account's worker if AutoSync is on, the service is open
// and the account has none yet.
func (s *Service) startWorker(id int64) {
	if !s.opts.AutoSync {
		return
	}
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	if s.closed || s.workers[id] != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	if s.workers == nil {
		s.workers = map[int64]*worker{}
	}
	s.workers[id] = w
	go func() {
		defer close(w.done)
		s.runWorker(ctx, id, w.wake)
		s.workersMu.Lock()
		if w.stopTimer != nil {
			w.stopTimer() // ctx is done: none is set again
		}
		s.workersMu.Unlock()
		if beforeWorkerExit != nil {
			beforeWorkerExit()
		}
	}()
}

// stopWorker stops the account's worker, if any, and waits for it to exit. The
// worker stays registered until then, so a concurrent Close waits for it too.
func (s *Service) stopWorker(id int64) {
	s.workersMu.Lock()
	w := s.workers[id]
	s.workersMu.Unlock()
	if w == nil {
		return
	}
	w.cancel()
	<-w.done
	s.workersMu.Lock()
	if s.workers[id] == w {
		delete(s.workers, id)
	}
	s.workersMu.Unlock()
}

// wakeWorker asks the account's worker for a sync now.
func (s *Service) wakeWorker(id int64) {
	s.workersMu.Lock()
	w := s.workers[id]
	s.workersMu.Unlock()
	if w != nil {
		select {
		case w.wake <- struct{}{}:
		default: // one is already pending
		}
	}
}

// wakeWorkerAt has the account's worker sync at t (on the Service's clock) for
// src, in place of the time src set before; a zero t sets none. The worker
// wakes at the earliest time of any source.
func (s *Service) wakeWorkerAt(id int64, src wakeSource, t time.Time) {
	s.workersMu.Lock()
	defer s.workersMu.Unlock()
	w := s.workers[id]
	if w == nil || w.ctx.Err() != nil {
		return
	}
	w.due[src] = t
	s.armWorker(id, w)
}

// armWorker sets w's timer for the earliest time a source wants. When it fires,
// the times it reached are spent, the timer is set for the rest and the worker
// syncs. Under workersMu.
func (s *Service) armWorker(id int64, w *worker) {
	if w.stopTimer != nil {
		w.stopTimer()
		w.stopTimer = nil
	}
	var next time.Time
	for _, t := range w.due {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if next.IsZero() {
		return
	}
	c := s.clk()
	w.stopTimer = c.AfterFunc(next.Sub(c.Now()), func() {
		s.workersMu.Lock()
		if w.ctx.Err() == nil {
			now := c.Now()
			for i, t := range w.due {
				if !t.After(now) {
					w.due[i] = time.Time{}
				}
			}
			s.armWorker(id, w)
		}
		s.workersMu.Unlock()
		s.wakeWorker(id)
	})
}

func (s *Service) runWorker(ctx context.Context, id int64, wake chan struct{}) {
	var delay, pace time.Duration
	for {
		if !s.awaitCredentials(ctx, id, wake) {
			return
		}
		chatter, err := s.syncRound(ctx, id, wake)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			delay = 0
			if !chatter {
				pace = 0
				continue
			}
			// The first chatter goes on at once (it may be real mail); a run of it
			// waits 2 s, 4 s, ... up to backoffMax. A local change cuts the wait short.
			p := pace
			pace = min(max(2*pace, backoffMin), backoffMax)
			if p > 0 && !waitOrWake(ctx, p, wake) {
				return
			}
			continue
		}
		delay = min(max(2*delay, backoffMin), backoffMax)
		if !waitOrWake(ctx, delay, nil) { // a wake stays pending until the backoff is over
			return
		}
	}
}

// waitOrWake waits d, or until wake fires (a nil wake never does). It reports
// false when ctx ends.
func waitOrWake(ctx context.Context, d time.Duration, wake <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-wake:
	case <-ctx.Done():
		return false
	}
	return true
}

// awaitCredentials parks the worker while IMAP refuses the password or the
// keychain has none (status reauth_required; SMTP refusing it only pauses the
// outbox, and an unavailable keychain, status error, backs off instead), so it
// makes no login attempts. Any LOGIN that succeeds, as SyncNow's, clears the
// status and wakes it; so does any local change, which only re-reads the status. It reports
// false when ctx ends.
func (s *Service) awaitCredentials(ctx context.Context, id int64, wake chan struct{}) bool {
	for {
		r, err := s.db.Query(ctx, "SELECT status FROM accounts WHERE id = ?", id) // IMAP's; SMTP's never parks
		if err != nil || len(r.Values) == 0 || store.Str(r.Values[0][0]) != "reauth_required" {
			return ctx.Err() == nil // on a read error the round reports it
		}
		if onParked != nil {
			onParked(id)
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return false
		}
	}
}

// syncRound runs SyncNow and then waits for a reason to sync again. With IDLE,
// INBOX is selected on a second connection before SyncNow, so mail that lands
// during or after the sync ends the IDLE that follows. chatter reports that a
// server update ended IDLE within chatterWithin of its start.
func (s *Service) syncRound(ctx context.Context, id int64, wake chan struct{}) (chatter bool, err error) {
	updates := make(chan struct{}, 1)
	notify := func() {
		select {
		case updates <- struct{}{}:
		default:
		}
	}
	c, release, _, err := s.dialAccount(ctx, id, &imapclient.UnilateralDataHandler{
		Expunge: func(uint32) { notify() },
		Mailbox: func(d *imapclient.UnilateralDataMailbox) {
			if d.NumMessages != nil { // FLAGS and PERMANENTFLAGS change no message
				notify()
			}
		},
		Fetch: func(*imapclient.FetchMessageData) { notify() },
	})
	if err != nil {
		return false, s.workerFailed(ctx, id, err)
	}
	defer release()
	idle := c.Caps().Has(imap.CapIdle)
	if idle {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			return false, s.workerFailed(ctx, id, err)
		}
	} else {
		release() // only needed for IDLE; a second release is harmless
	}
	if err := s.syncNow(ctx, id, false, s.light.Load()); err != nil {
		return false, err // syncNow announced it
	}
	if idle {
		start := time.Now()
		byServer, err := mailsync.Idle(ctx, c, updates, wake, idleMax)
		if err != nil {
			return false, s.workerFailed(ctx, id, err)
		}
		return byServer && time.Since(start) < chatterWithin, nil
	}
	t := time.NewTimer(pollInterval)
	defer t.Stop()
	select {
	case <-t.C:
	case <-wake:
	case <-ctx.Done():
	}
	return false, nil
}

// workerFailed announces a failure outside SyncNow, unless the worker is stopping.
func (s *Service) workerFailed(ctx context.Context, id int64, err error) error {
	if ctx.Err() == nil {
		s.bus.Publish(events.Event{Type: events.SyncFailed, AccountID: id, Error: err.Error(), Kind: string(conn.Classify(err))})
	}
	return err
}
