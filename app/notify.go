package app

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/internal/i18n"
)

// notifyWait is how long new mail gathers before reading it for notifications.
const notifyWait = time.Second

// arrivals are an account's new messages and their threads awaiting notifications.
type arrivals struct{ msgs, threads []int64 }

// mailArrived gathers ev's new messages for their notifications,
// shown notifyWait after the first of them.
func (a *App) mailArrived(ev events.Event) {
	if len(ev.MessageIDs) == 0 {
		return
	}
	if a.newMail == nil {
		a.newMail = map[int64]*arrivals{}
	}
	acc := ev.AccountID
	got := a.newMail[acc]
	if got == nil {
		got = &arrivals{}
		a.newMail[acc] = got
		a.after(notifyWait, func() {
			delete(a.newMail, acc)
			a.notify(acc, got)
		})
	}
	got.msgs = append(got.msgs, ev.MessageIDs...)
	got.threads = append(got.threads, ev.ThreadIDs...)
}

// notify moves the account's inbox arrivals from blocked senders to Junk, and
// shows a notification for each of the others, unless disabled or the window
// is in front. Preview shows the sender, subject and snippet; otherwise it
// says only "Godwit" and "new mail".
func (a *App) notify(acc int64, got *arrivals) {
	st := a.settings
	show := st.NotifyNewMail && !a.foreground
	texts := i18n.In(i18n.Language())
	plat, svc, logf := a.plat, a.svc, a.logf
	codeBanner, now := st.NotifyPreview && plat.SignInCodeBannerSupported(), a.clock.Now()
	a.do(func(ctx context.Context) error {
		msgs, err := inboxArrivals(ctx, svc, acc, got)
		if err != nil {
			logf("notify: account %d: reading new mail: %v", acc, err)
			return nil
		}
		blockedSenders, err := svc.BlockedSenders(ctx) // the stored list: a block takes effect at once
		if err != nil {
			logf("notify: account %d: reading blocked senders: %v", acc, err)
		}
		var junk []int64
		msgs = slices.DeleteFunc(msgs, func(m core.MessageSummary) bool {
			blocked := slices.Contains(blockedSenders, mail.AddrKey(m.From))
			if blocked {
				junk = append(junk, m.ID)
			}
			return blocked
		})
		if len(junk) > 0 {
			// Never deleted instead: a failed move leaves the mail in the inbox, unnotified.
			if err := svc.MoveToRole(ctx, junk, "junk"); err != nil {
				logf("notify: account %d: moving blocked senders' mail to junk: %v", acc, err)
			}
		}
		if !show || !plat.NotificationsSupported() {
			return nil
		}
		if codeBanner {
			// Only the newest code takes the banner; older ones in the same batch
			// keep their ordinary notification.
			if codes, err := svc.RecentSignInCodes(ctx, now); err == nil && len(codes) > 0 {
				msgs = slices.DeleteFunc(msgs, func(m core.MessageSummary) bool { return m.ID == codes[0].MessageID })
			}
		}
		for _, m := range msgs {
			n := Notification{ID: "message:" + strconv.FormatInt(m.ID, 10), MessageID: m.ID, Title: "Godwit", Body: texts.NotifyNewMail()}
			if st.NotifyPreview {
				n.Title, n.Subtitle, n.Body = displayName(m.From), m.Subject, m.Snippet
				n.Sender, n.ThreadID = m.From, m.ThreadID
			}
			if err := plat.Notify(n); err != nil {
				logf("notify: account %d: %v", acc, err)
			}
		}
		return nil
	}, nil)
}

// inboxArrivals are the arrived messages that are in an inbox-role folder.
func inboxArrivals(ctx context.Context, svc *core.Service, acc int64, got *arrivals) ([]core.MessageSummary, error) {
	fs, err := svc.ListFolders(ctx, acc)
	if err != nil {
		return nil, err
	}
	inbox := map[int64]bool{}
	for _, f := range fs {
		if f.Role == "inbox" {
			inbox[f.ID] = true
		}
	}
	want := map[int64]bool{}
	for _, id := range got.msgs {
		want[id] = true
	}
	var out []core.MessageSummary
	for _, tid := range slices.Compact(slices.Sorted(slices.Values(got.threads))) {
		th, err := svc.GetThread(ctx, tid)
		if err != nil {
			return nil, err
		}
		for _, m := range th.Messages {
			if want[m.ID] && inbox[m.FolderID] {
				delete(want, m.ID) // a message in two gathered threads counts once
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// NotificationClicked shows the window with what a notification was about:
// its message, or the account's inbox for a gathered one. Only the newest
// click navigates, and only if the route has not changed meanwhile; a click
// before the first load waits for it.
func (a *App) NotificationClicked(id string) {
	a.plat.ShowWindow()
	if !a.loaded {
		a.noteFirst = id
		return
	}
	a.noteGen++
	gen, look := a.noteGen, a.noteRoute
	var r Route
	var ok bool
	a.query(func(ctx context.Context) (err error) {
		r, ok, err = look(ctx, id)
		return err
	}, func(err error) {
		switch {
		case gen != a.noteGen:
		case err != nil:
			a.toastErr(err)
		case ok:
			if a.settings.ThreadView {
				r.MessageID = 0 // the whole thread, as the list opens it
			}
			a.navigate(r)
		}
	})
}

// notificationRoute is where the notification id leads (MessageID set for a
// message); false for an id of no known kind or an account without an inbox.
func (a *App) notificationRoute(ctx context.Context, id string) (Route, bool, error) {
	kind, num, _ := strings.Cut(id, ":")
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return Route{}, false, nil
	}
	switch kind {
	case "message":
		m, err := a.svc.GetMessageText(ctx, n) // no inline image is downloaded for a route
		if err != nil {
			return Route{}, false, err
		}
		return Route{Page: PageMail, View: core.View{FolderID: m.FolderID}, ThreadID: m.ThreadID, MessageID: m.ID}, true, nil
	case "account":
		fs, err := a.svc.ListFolders(ctx, n)
		if err != nil {
			return Route{}, false, err
		}
		if i := slices.IndexFunc(fs, func(f core.Folder) bool { return f.Role == "inbox" }); i >= 0 {
			return Route{Page: PageMail, View: core.View{FolderID: fs[i].ID}}, true, nil
		}
	}
	return Route{}, false, nil
}

// showUnread updates the app icon and tray according to their settings.
func (a *App) showUnread(inbox int) {
	badge := inbox
	if !a.settings.UnreadOnAppIcon {
		badge = 0
	}
	if badge != a.appUnread {
		a.appUnread = badge
		a.plat.SetAppUnread(badge)
	}
	if !a.settings.UnreadInMenuBar || !a.settings.ShowTrayIcon {
		inbox = 0
	}
	if inbox != a.trayUnread {
		a.trayUnread = inbox
		a.plat.SetTrayUnread(inbox)
	}
}

// Compose starts a new draft in a compose window, as the sidebar's button does.
func (a *App) Compose() { a.mail.compose() }

// HideTrayIcon hides the menu bar icon by updating the setting. Its menu is
// mostly used with the window hidden, so the window comes up to show the toast.
func (a *App) HideTrayIcon() {
	a.onUI(func() {
		st := a.settings
		if !st.ShowTrayIcon {
			return
		}
		st.ShowTrayIcon = false
		a.changeSettings(st)
		a.plat.ShowWindow()
		a.toast(i18n.T("tray.hidden_toast"))
	})
}

// SyncAll syncs every account now. The status shows syncing until the last one
// returns; a click while that batch is in flight starts nothing more.
func (a *App) SyncAll() {
	if a.syncBatch != nil || len(a.accounts) == 0 {
		return
	}
	b := &syncBatch{pending: make(map[int64]bool, len(a.accounts)), failed: map[int64]bool{}}
	for _, acc := range a.accounts {
		b.pending[acc.ID] = true
	}
	a.syncBatch = b
	a.tick()
	for _, acc := range a.accounts {
		id := acc.ID
		a.do(func(ctx context.Context) error { return a.svc.SyncNow(ctx, id) }, func(err error) { a.onBatchDone(id, err) })
	}
}

// onBatchDone records one account's SyncNow returning. One no longer pending
// was removed meanwhile, so its error is not shown; nor is a cancelled one's, as
// removing the account cancels it.
func (a *App) onBatchDone(id int64, err error) {
	if !a.batchPending(id) {
		return
	}
	delete(a.syncBatch.pending, id)
	if err != nil && !errors.Is(err, context.Canceled) {
		a.syncBatch.failed[id] = true
		a.toastErr(err)
	}
	a.finishBatch()
}

// finishBatch ends the Sync All once no account is pending, setting the last
// sync time when none failed.
func (a *App) finishBatch() {
	if a.syncBatch == nil || len(a.syncBatch.pending) > 0 {
		return
	}
	if len(a.syncBatch.failed) == 0 {
		a.syncedAt = a.clock.Now()
	}
	a.syncBatch = nil
	a.tick()
}

// batchPending reports whether the Sync All in flight still waits on the account.
func (a *App) batchPending(id int64) bool { return a.syncBatch != nil && a.syncBatch.pending[id] }

// LightBackground reports the setting the settings page and the tray show.
func (a *App) LightBackground() bool { return a.settings.LightBackground }

// SetLightBackground turns the setting on or off, as the settings page's
// switch does; the tray's checkbox calls it.
func (a *App) SetLightBackground(on bool) {
	if a.settings.LightBackground == on {
		return
	}
	st := a.settings
	st.LightBackground = on
	a.changeSettings(st)
}

// KeepInBackground reports whether closing the window only hides it: 后台常驻
// is on, or an enabled rule runs in the background.
func (a *App) KeepInBackground() bool {
	if a.settings.KeepInBackground {
		return true
	}
	on, _ := a.svc.RulesInBackground(context.Background()) // an error quits, as with no such rule
	return on
}
