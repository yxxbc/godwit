// Package app is Godwit's desktop interface: one App whose view is rebuilt
// every frame from state that only the UI thread changes.
package app

import (
	"cmp"
	"context"
	"errors"
	"log"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/kit"
	"github.com/godwit-email/godwit/internal/mcpclients"
)

// Platform is what the App needs from the operating system. OpenFiles,
// OpenDirectory and SaveFile block: call them only off the UI thread.
type Platform interface {
	OpenURL(url string) error
	OpenPath(path string) error
	OpenFiles(multiple bool) ([]string, error)
	OpenImage() (string, error)
	OpenDirectory() (string, error)
	SaveFile(defaultName string) (string, error)
	Notify(n Notification) error
	NotificationsSupported() bool
	SetOpenAtLogin(on bool) error
	SetTrayVisible(visible bool)
	SetTrayUnread(n int)              // the inboxes' unread total on the tray; 0 clears it
	SetAppUnread(n int)               // the inboxes' unread total on the app icon; 0 clears it
	SetSignInCodes([]core.SignInCode) // replaces the tray's recent codes; UI thread
	ShowSignInCode(*core.SignInCode)  // a nonactivating copy card; nil dismisses it; UI thread
	SignInCodeBannerSupported() bool  // UI thread; false keeps ordinary mail notifications
	ShowWindow()                      // shows the window, in front
	SetThemeSource(theme string)
	TempDir() string
	// TypeIcon is the icon the file manager shows for files with extension
	// ext (no dot), as a size by size pixel PNG. Blocks: call it off the UI
	// thread.
	TypeIcon(ext string, size int) ([]byte, error)
	// SetLocalAPI starts (on) or stops the Local API server for request
	// gen, returning its socket's path while on. Off when it fails. Blocks:
	// call off the UI thread.
	SetLocalAPI(gen uint64, on bool) (path string, err error)
	// DataDir is the data directory; custom: it is not the default one.
	DataDir() (dir string, custom bool)
	// OpenCompose shows draftID in a compose window of its own.
	OpenCompose(draftID int64)
	// ComposeOpen reports whether draftID's compose window is open or opening.
	// The platform tells the App when that changes: ComposeWindowsChanged.
	ComposeOpen(draftID int64) bool
	// DraftDeleted is told draft id was deleted here: its compose window,
	// if one is open, closes.
	DraftDeleted(id int64)
	// SettingsChanged is told the settings after every change.
	SettingsChanged(st core.Settings)
	// NewBodyView makes a reader page in the main window showing message's
	// body (bound to it before it loads); close unbinds and destroys it.
	// The page reports to BodyHeight, BodyLink, BodyNavigate, BodyBuilt and
	// BodyFailed with the view. UI thread only; it may run the event loop.
	NewBodyView(message int64) (view ui.NativeWebView, close func(), err error)
	// AllowImages lets the pages showing message load its remote images
	// (加载图片). Call it after NewBodyView: it forgets once the last page
	// of the message closes.
	AllowImages(message int64)
	// BodyGlass tells a body's page how much of it, in CSS pixels, the
	// reader's bars cover from its top and from its bottom: the page blurs
	// there. UI thread only.
	BodyGlass(view ui.NativeWebView, top, bottom int)
	// TranslateBody replaces only the body's text nodes; nil shows the original.
	TranslateBody(message int64, texts map[string]string)
	// Print prints the message's body page. UI thread only.
	Print(message int64)
	// NewSignatureView makes the signature page (a signature's rich-text
	// editor, in Settings › Signatures) in the main window; the App makes
	// one and closes it (unregistered and destroyed) as it closes. show
	// switches it to signature id: the page saves the shown one's edit as
	// that one first. flush asks the page to save now; done hears whether
	// it did, on the UI thread, at most once. UI thread only.
	NewSignatureView() (view ui.NativeWebView, show func(id int64), flush func(done func(ok bool)), close func(), err error)
	// NewBirdView makes the About page's 3D bird in the main window; close
	// destroys it. UI thread only.
	NewBirdView() (view ui.NativeWebView, close func(), err error)
	// CheckForUpdates checks now; the updater's own window shows the result
	// and installs a newer version. The Store's package opens its Store page.
	CheckForUpdates()
	// LatestVersion asks the update feed (the Store's catalog for the
	// Store's package) for a version newer than this one, without a
	// window: "" when up to date. Blocks: call off the UI thread.
	LatestVersion(ctx context.Context) (string, error)
	// UpdatesInstallThemselves says whether a newer version installs without
	// asking (the updater's automatic downloads): it waits for a relaunch.
	// It may read a file: call off the UI thread.
	UpdatesInstallThemselves() bool
	// RepoStarred asks the GitHub CLI whether the user starred Godwit's
	// repository: ok is false without gh signed in. Blocks.
	RepoStarred(ctx context.Context) (starred, ok bool)
	// StarRepo stars it with the GitHub CLI. Blocks.
	StarRepo(ctx context.Context) error
}

// Notification is a system notification about a message.
type Notification struct {
	ID, Title, Subtitle, Body string
	MessageID                 int64
	Sender                    string // only set when preview is enabled
	ThreadID                  int64
}

// OAuthFlow signs in with an OAuth provider; the default wraps the Service.
type OAuthFlow interface {
	Begin(ctx context.Context, provider, loginHint string, client core.OAuthClient) (flowID, authURL string, err error)
	Await(ctx context.Context, flowID string) (email string, err error)
	Cancel(flowID string)
}

type serviceOAuth struct{ svc *core.Service }

func (o serviceOAuth) Begin(ctx context.Context, provider, loginHint string, client core.OAuthClient) (string, string, error) {
	return o.svc.BeginOAuth(ctx, provider, loginHint, client)
}
func (o serviceOAuth) Await(ctx context.Context, flowID string) (string, error) {
	return o.svc.AwaitOAuth(ctx, flowID)
}
func (o serviceOAuth) Cancel(flowID string) { o.svc.CancelOAuth(flowID) }

// Deps are the App's collaborators. Post runs a function on the UI thread
// (win.Update in production). Clock and OAuth default to the real ones.
type Deps struct {
	Svc      *core.Service
	Settings core.Settings
	Platform Platform
	Post     func(func())
	Clock    Clock
	OAuth    OAuthFlow

	// SystemLanguage is the system's language tag; the interface uses it
	// while Settings.UILanguage is "".
	SystemLanguage string
	// SetLanguage applies the UI preference; an empty tag follows the system.
	// By default only the catalog changes; the desktop also updates MyGo.
	SetLanguage func(string)

	// Vibrancy: the window has a material, which shows around the panel.
	Vibrancy bool

	// Logf logs what fails out of the user's sight (log.Printf by default).
	// Lines carry no secrets and no mail content.
	Logf func(format string, args ...any)

	// Dev: the build is a development one: the rail offers the Kit gallery.
	Dev bool
}

// Page is a top-level screen.
type Page int

const (
	PageMail Page = iota
	PageWit
	PageSettings
	PageOnboarding
	PageKit // dev builds only
	PageContacts
)

// Route is where the App is.
type Route struct {
	Page                                Page
	View                                core.View
	ThreadID, MessageID, ConversationID int64
	Settings                            string
	Contact                             string // the contact the contacts page shows, by address
}

// syncBatch is a Sync All in flight: it counts as a sync only once every account finished without failing.
type syncBatch struct {
	pending map[int64]bool // accounts whose SyncNow has not returned
	failed  map[int64]bool // accounts whose SyncNow failed
}

// App is the interface's state. Every field is read and written on the UI thread.
type App struct {
	svc   *core.Service
	plat  Platform
	post  func(func())
	clock Clock
	oauth OAuthFlow

	ctx      context.Context // cancelled by Close or Start's ctx
	cancel   context.CancelFunc
	pumpDone chan struct{} // closed when the event pump has stopped and unsubscribed

	route Route
	gen   uint64 // bumped on every navigation; async results from an older gen are dropped

	fetch         func(ctx context.Context) (core.Settings, []core.Account, error) // what load reads; tests swap it
	loadGen       uint64                                                           // bumped per load; older completions are dropped
	loaded        bool
	loadErr       error // the newest load's error; nil once a load succeeds
	loadFailing   bool  // loads have failed since the last success: toast once per streak
	accounts      []core.Account
	labels        []core.Label           // in the user's order, as the last load read them
	accountLabels map[int64][]core.Label // labels with counts scoped to each account
	signatures    []core.Signature       // oldest first, as the last load read them
	trusted       []string               // the senders whose remote images load at once, as the last load read them
	blocked       []string               // the blocked senders, as the last load read them or a block changed them
	settings      core.Settings          // what the interface shows: the newest edit while one saves
	taskModels    map[string]bool        // the reader's tasks ("summarize", "translate", "" quick replies) core has a model for, and "labels"
	taskGen       uint64                 // bumped per refreshTasks; only the newest answer counts

	settingsOK       core.Settings                                                      // the last settings the Service confirmed; a failed save reverts to them
	settingsGen      uint64                                                             // bumped per settings save; only the newest one's result counts
	settingsVer      uint64                                                             // bumped per local settings change
	settingsSavedVer uint64                                                             // the settingsVer whose save the Service confirmed (or reverted to)
	saveSettings     func(ctx context.Context, st core.Settings) (core.Settings, error) // tests swap it
	mcpEnv           func() (mcpclients.Env, error)                                     // where 接入 AI 应用 looks for the AI apps; tests swap it
	setLanguage      func(string)
	sysLang          string // Deps.SystemLanguage
	accountsGen      uint64 // the load gen that read a.accounts

	localAPIWant bool   // what the last SetLocalAPI asked for
	localAPIGen  uint64 // bumped per SetLocalAPI; only the newest one's result counts
	localAPIPath string // the Local API's socket while it serves
	localAPIErr  error  // why the Local API last failed; the privacy page shows it

	foreground                           bool
	vibrancy                             bool                // Deps.Vibrancy
	newMail                              map[int64]*arrivals // per account, what arrived for its pending notifications
	logf                                 func(format string, args ...any)
	noteRoute                            func(ctx context.Context, id string) (Route, bool, error) // where a notification leads; tests wrap it
	noteGen                              uint64                                                    // bumped per notification click; only the newest navigates
	noteFirst                            string                                                    // a click that came before the first load
	inboxUnread                          int                                                       // the inboxes' unread total
	trayUnread                           int                                                       // what the tray shows
	appUnread                            int                                                       // what the app icon shows; -1 before the first update
	codes                                []core.SignInCode
	codesReady, codesLoading, codesAgain bool
	codesStop                            func()
	codesStarted                         time.Time

	history, ahead []Route // navigation: routes behind and, after back, ahead
	dev            bool
	pages          map[Page]pageView
	sidebarHidden  bool
	// sidebarPull is how far a drag has gone past the sidebar's least width,
	// and sidebarDragFrom the width the drag started from (0: no drag).
	sidebarPull, sidebarDragFrom float32
	sidebarW                     float32 // the sidebars' width as dragged, shared by every page; 0 until the first panel
	listW                        float32 // the mail list's width as dragged; 0 until the mail page shows
	sidebarSave                  func()  // stops the pending save of a dragged width
	latest                       string  // a newer version the update feed offers (watchLatest); "" none
	latestInstalls               bool    // it installs itself and waits for a relaunch

	dirty             []events.Event // events since the last refresh
	refreshStop       func()         // the pending throttled refresh; nil when none
	events, refreshes int            // counted for tests: refreshes stay far fewer than events

	syncBatch  *syncBatch          // the Sync All in flight; nil when none
	syncing    map[int64]bool      // accounts syncing now
	syncErr    map[int64]syncFail  // each account's failing sync, until it syncs
	syncOpen   bool                // the sync status's account list shows
	syncedAt   time.Time           // the last successful sync; zero before one
	syncedAtBy map[int64]time.Time // each account's last successful sync since launch
	ticking    bool                // a minute tick redraws the sync age

	addAccount *onboarding // the add-account sheet; nil when closed
	mail       *mailPage   // the Mail section
	palette    palette     // the ⌘K search panel
	writes     *writeQueue // writes to the Service, ordered per resource

	closeAsk    bool   // a write failed or is slow as the window closes: ask
	closeErr    error  // why
	closeAnyway func() // closes after all

	witView    ui.NativeWebView                                  // the Wit page; nil before SetWitPage
	witOpen    func(conv int64) (version int64)                  // tells the Wit page the route's conversation
	witCreated func(conv int64) (version int64)                  // the same, for the conversation its Ask made
	witAsk     func(query string, quoted ...core.MessageSummary) // hands a question, quoting mail, to the Wit page, which asks it

	link     string // the link waiting for confirmation
	linkOpen bool   // the link dialog shows

	mcpSend mcpSendState // godwit mcp's send request waiting for the user (mcpsend.go)

	customIcons map[int64]accountImage    // by account id: its uploaded icon (accountImage)
	typeIcons   map[string]ui.ImageSource // by extension: the file manager's icon (typeIcon)

	tempMu   sync.Mutex
	tempDirs []string // made by tempDir; Close removes them

	unread      map[int64]int // per account, summed over its folders
	toasts      []ui.Toast    // shown on the next frame
	closeToasts []string      // the IDs of toasts to close on the next frame

	undoToasts map[int64]bool // outbox items offered undo send: true while the toast shows
	undoGen    uint64         // bumped per offerUndo; only the newest listing counts
}

// New builds an App; Start brings it up.
func New(d Deps) *App {
	a := &App{settings: d.Settings, settingsOK: d.Settings, setLanguage: d.SetLanguage, svc: d.Svc, plat: d.Platform, post: d.Post, clock: d.Clock, oauth: d.OAuth, sysLang: d.SystemLanguage, vibrancy: d.Vibrancy, appUnread: -1, mcpEnv: mcpclients.Local}
	kit.Glass = d.Vibrancy // dialogs frost their panels over a window on a material
	if a.setLanguage == nil {
		a.setLanguage = func(tag string) { i18n.SetLanguage(cmp.Or(tag, a.sysLang)) }
	}
	if a.clock == nil {
		a.clock = realClock{}
	}
	if a.logf = d.Logf; a.logf == nil {
		a.logf = log.Printf
	}
	a.noteRoute = a.notificationRoute
	if a.oauth == nil {
		a.oauth = serviceOAuth{d.Svc}
	}
	a.fetch = func(ctx context.Context) (core.Settings, []core.Account, error) {
		accs, err := a.svc.ListAccounts(ctx)
		if err != nil {
			return core.Settings{}, nil, err
		}
		st, err := a.svc.GetSettings(ctx)
		return st, accs, err
	}
	a.saveSettings = a.svc.SetSettings
	a.writes = newWriteQueue(a)
	a.mail = newMailPage(a)
	a.pages = map[Page]pageView{PageMail: a.mail, PageWit: newAskPage(a), PageSettings: newSettingsPage(a), PageContacts: newContactsPage(a)}
	a.pages[PageOnboarding] = newOnboarding(a, false)
	a.pages[PageKit] = &kitPage{}
	a.dev = d.Dev
	a.syncing, a.syncErr, a.syncedAtBy = map[int64]bool{}, map[int64]syncFail{}, map[int64]time.Time{}
	a.undoToasts = map[int64]bool{}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.pumpDone = make(chan struct{})
	return a
}

// Start loads accounts and settings and starts the event pump. Cancelling
// ctx closes the App's work as Close does.
func (a *App) Start(ctx context.Context) {
	a.codesStarted = a.clock.Now().Truncate(time.Second)
	context.AfterFunc(ctx, a.cancel)
	ch, unsub := a.svc.Subscribe()
	go func() {
		defer close(a.pumpDone)
		defer unsub()
		for {
			select {
			case <-a.ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				a.onUI(func() { a.onEvent(ev) })
			}
		}
	}()
	a.load()
	a.after(latestFirst, a.watchLatest)
}

// latestFirst leaves the updater's own check at launch, which may start
// installing, its head start; latestEvery spaces the checks after it.
const latestFirst, latestEvery = time.Minute, 6 * time.Hour

// watchLatest reads the newer version now and every latestEvery after.
func (a *App) watchLatest() {
	a.readLatest()
	a.after(latestEvery, a.watchLatest)
}

// readLatest asks the update feed for a newer version, for the sidebar's
// update card and About's button.
func (a *App) readLatest() {
	var v string
	var auto bool
	a.do(func(ctx context.Context) (err error) {
		v, err = a.plat.LatestVersion(ctx)
		auto = a.plat.UpdatesInstallThemselves()
		return err
	}, func(err error) {
		if err == nil {
			a.latest, a.latestInstalls = v, auto
		} // a dev build, offline: Check for Updates says why
	})
}

// load re-reads accounts, settings and unread counts; only the newest
// load's result applies. The first load picks the start route.
func (a *App) load() {
	a.loadGen++
	gen := a.loadGen
	var accs []core.Account
	var st core.Settings
	var unread map[int64]int
	var labels []core.Label
	var accLabels map[int64][]core.Label
	var sigs []core.Signature
	var trusted, blocked []string
	listsRead := false
	inbox := 0
	fetch, ver := a.fetch, a.settingsVer
	settled := ver == a.settingsSavedVer // no local change waits for its save
	a.do(func(ctx context.Context) error {
		var err error
		if st, accs, err = fetch(ctx); err != nil {
			return err
		}
		labels, _ = a.svc.ListLabels(ctx) // chips only: a failure keeps the last ones
		accLabels, _ = a.svc.AllAccountLabels(ctx)
		sigs, _ = a.svc.ListSignatures(ctx) // likewise
		// The sender lists too: listsRead says whether to take them.
		trusted, err = a.svc.TrustedImageSenders(ctx)
		if err == nil {
			blocked, err = a.svc.BlockedSenders(ctx)
		}
		listsRead = err == nil
		unread = map[int64]int{}
		for _, acc := range accs {
			fs, err := a.svc.ListFolders(ctx, acc.ID)
			if err != nil {
				return err
			}
			unread[acc.ID] = 0 // every account has a key, folders or not
			for _, f := range fs {
				if f.Role == "junk" || f.Role == "trash" { // as the sidebar's folder rows count
					continue
				}
				unread[acc.ID] += f.Unread
				if f.Role == "inbox" {
					inbox += f.Unread
				}
			}
		}
		return nil
	}, func(err error) {
		if gen != a.loadGen {
			return
		}
		a.loadErr = err
		if err != nil {
			if a.loaded && !a.loadFailing { // the first load's error shows in place of the shell
				a.toastErr(err)
			}
			a.loadFailing = true
			return
		}
		a.loadFailing = false
		first := !a.loaded
		for _, old := range a.accounts {
			if !slices.ContainsFunc(accs, func(acc core.Account) bool { return acc.ID == old.ID }) {
				a.mail.reader.invalidateAvatars(true)
				break
			}
		}
		a.accounts, a.accountsGen, a.unread, a.loaded = accs, gen, unread, true
		if b := a.syncBatch; b != nil { // a removed account no longer holds up or fails the batch
			for id := range b.pending {
				if !slices.ContainsFunc(accs, func(acc core.Account) bool { return acc.ID == id }) {
					delete(b.pending, id)
				}
			}
			for id := range b.failed {
				if !slices.ContainsFunc(accs, func(acc core.Account) bool { return acc.ID == id }) {
					delete(b.failed, id)
				}
			}
			a.finishBatch()
		}
		if listsRead {
			if !slices.Equal(a.trusted, trusted) {
				a.mail.reader.invalidateAvatars(true)
			}
			a.trusted, a.blocked = trusted, blocked
		}
		if sigs != nil {
			a.signatures = sigs
		}
		if labels != nil {
			a.labels = labels
			if a.mail != nil {
				a.mail.labelsLoaded()
			}
		}
		if accLabels != nil {
			a.accountLabels = accLabels
		}
		if settled && ver == a.settingsVer && ver == a.settingsSavedVer { // else st may predate a local change
			if a.settings.RemoteImages != st.RemoteImages {
				a.mail.reader.invalidateAvatars(true)
			}
			a.settings, a.settingsOK = st, st
			a.mail.reader.refreshAvatars()
		}
		a.syncLocalAPI()
		a.refreshTasks() // skills or services may have changed too
		a.inboxUnread = inbox
		a.showUnread(inbox)
		a.refreshSignInCodes()
		if first {
			a.setLanguage(st.UILanguage)
		}
		gone := func(id int64, _ bool) bool { _, ok := unread[id]; return !ok } // a removed account
		maps.DeleteFunc(a.syncing, gone)
		maps.DeleteFunc(a.syncErr, func(id int64, _ syncFail) bool { return gone(id, false) })
		switch {
		case len(accs) == 0 && a.route.Page != PageOnboarding:
			a.history, a.ahead = nil, nil
			a.setRoute(Route{Page: PageOnboarding})
		case len(accs) > 0 && first:
			a.setRoute(Route{Page: PageMail, View: core.View{Unified: "inbox"}})
		}
		if id := a.noteFirst; first && id != "" {
			a.noteFirst = ""
			a.NotificationClicked(id)
		}
	})
}

// Close cancels all work in flight, whose done callbacks never run, closes
// the reader's body pages and removes the temporary copies of opened
// attachments. UI thread.
func (a *App) Close() {
	a.cancel() // the event pump unsubscribes as it stops
	if a.codesStop != nil {
		a.codesStop()
	}
	a.plat.ShowSignInCode(nil)
	a.mail.reader.closeViews()
	a.pages[PageSettings].(*settingsPage).closeSignature()
	a.pages[PageSettings].(*settingsPage).closeBird()
	a.tempMu.Lock()
	defer a.tempMu.Unlock()
	for _, d := range a.tempDirs {
		os.RemoveAll(d)
	}
	a.tempDirs = nil
}

// tempDir makes a new directory in parent ("" for the system's) that Close
// removes. Safe off the UI thread.
func (a *App) tempDir(parent string) (string, error) {
	a.tempMu.Lock()
	defer a.tempMu.Unlock()
	if err := a.ctx.Err(); err != nil {
		return "", err
	}
	d, err := os.MkdirTemp(parent, "godwit-")
	if err == nil {
		a.tempDirs = append(a.tempDirs, d)
	}
	return d, err
}

// flushWait is how long BeforeClose waits for the writes before it asks.
const flushWait = 3 * time.Second

// errFlushSlow is why BeforeClose asks when the writes take longer than flushWait.
var errFlushSlow = errors.New("still saving")

// errViewUnsaved is why BeforeClose asks while the view editor has changes.
var errViewUnsaved = errors.New("the view was not saved")

// BeforeClose saves the signature page's edit and the settings' account
// fields, waits for every write in
// flight, then calls proceed: before the window closes or the App quits
// (compose windows have flushed their drafts by then, on their own). When
// a write fails, or they have not finished within flushWait, the window
// shows and asks whether to close anyway; writes that finish while it asks
// close after all.
func (a *App) BeforeClose(proceed func()) {
	if a.mail.viewUnsaved() {
		a.closeErr, a.closeAsk = errViewUnsaved, true
		a.closeAnyway = func() {
			a.mail.closeViewEditor()
			a.BeforeClose(proceed)
		}
		a.plat.ShowWindow()
		return
	}
	settled := false
	ask := func(err error) {
		settled = true
		a.closeErr, a.closeAnyway, a.closeAsk = err, proceed, true
		a.plat.ShowWindow()
	}
	stop := a.after(flushWait, func() {
		if !settled {
			ask(errFlushSlow)
		}
	})
	settle := func(err error) {
		stop()
		switch {
		case err == nil && (!settled || a.closeAsk && (errors.Is(a.closeErr, errFlushSlow) || a.closeErr == errSignatureUnsaved)):
			settled, a.closeAsk = true, false
			proceed()
		case !settled:
			ask(err)
		}
	}
	mark := a.writes.Mark() // every write failing from here on counts
	p := a.pages[PageSettings].(*settingsPage)
	p.flushSignature(func(sigErr error) {
		p.commitEdits()
		a.writes.Idle(func() {
			err := errors.Join(a.writes.ErrorsSince(mark)...)
			if err == nil {
				err = sigErr // alone, so a late save can settle it
			} else if sigErr != nil {
				err = errors.Join(err, sigErr)
			}
			settle(err)
		})
	})
}

// SetForeground records whether the window is in front.
func (a *App) SetForeground(on bool) {
	a.foreground = on
	if on {
		a.plat.ShowSignInCode(nil)
	}
	a.applyLightBackground(a.settings)
}

// applyLightBackground narrows the Service's syncs to the inbox while the
// window is behind another one, if st asks for it.
func (a *App) applyLightBackground(st core.Settings) {
	a.svc.SetLightBackground(st.LightBackground && !a.foreground)
}

// OnUI posts f to the UI thread unless the App has closed by then, for
// callbacks from elsewhere (the Wit page's).
func (a *App) OnUI(f func()) { a.onUI(f) }

// ComposeWindowsChanged is told a compose window opened or went (on the UI
// thread): the drafts list is read again, and the pane follows its draft.
func (a *App) ComposeWindowsChanged() {
	if _, ok := draftsOf(a.mail.folders, a.mail.view); ok {
		a.mail.reload()
	}
}

// onUI posts f to the UI thread unless the App has closed by then.
func (a *App) onUI(f func()) {
	a.post(func() {
		if a.ctx.Err() == nil {
			f()
		}
	})
}

// do runs fn on a goroutine and then done on the UI thread, unless Close
// cancelled fn's context first.
func (a *App) do(fn func(ctx context.Context) error, done func(err error)) {
	ctx := a.ctx
	go func() {
		err := fn(ctx)
		if done != nil {
			a.onUI(func() { done(err) })
		}
	}()
}

// refreshTasks asks core which of the reader's tasks resolve a model, after
// a load, a settings save and a model service change. Without a model they
// are hidden; any other error shows them, so their run reports it.
func (a *App) refreshTasks() {
	a.taskGen++
	gen, got := a.taskGen, map[string]bool{}
	a.do(func(ctx context.Context) error {
		for _, task := range []string{"summarize", "translate", ""} {
			_, err := a.svc.TaskModel(ctx, task)
			got[task] = !errors.Is(err, core.ErrAINotConfigured) && !errors.Is(err, core.ErrModelServiceNotFound) && !errors.Is(err, core.ErrModelNotFound)
		}
		got["labels"] = a.svc.LabelModelReady(ctx)
		return nil
	}, func(error) {
		if gen != a.taskGen || maps.Equal(a.taskModels, got) {
			return
		}
		a.taskModels = got
		if a.route.Page == PageMail {
			a.mail.reader.startAI() // what became available starts; each part runs once
		}
	})
}

// query is do for a read whose result belongs to the current route: done is
// dropped if the App navigated meanwhile.
func (a *App) query(fn func(ctx context.Context) error, done func(err error)) {
	gen := a.gen
	a.do(fn, func(err error) {
		if gen == a.gen {
			done(err)
		}
	})
}

// after runs f on the UI thread once d has passed, unless stop was called
// before f ran.
func (a *App) after(d time.Duration, f func()) (stop func()) {
	stopped := false // UI thread only
	cancel := a.clock.AfterFunc(d, func() {
		a.onUI(func() {
			if !stopped {
				f()
			}
		})
	})
	return func() { stopped = true; cancel() }
}
