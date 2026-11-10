package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	mygoi18n "github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/app"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/ai"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/theme"
	"github.com/godwit-email/godwit/ownerlock"
	"github.com/godwit-email/godwit/webui"
)

// trafficLightSize is the height of macOS's window buttons; trafficLightInset
// is how far the close button sits from the window's left edge.
const trafficLightSize, trafficLightInset = 14, 16

// windowOptions is the main window on goos.
func windowOptions(goos string) mygo.WindowOptions {
	o := mygo.WindowOptions{Title: "Godwit", Width: 1280, Height: 820, MinWidth: 960, MinHeight: 600, StateKey: "main",
		TitleBarStyle: mygo.TitleBarHiddenInset}
	switch goos {
	case "darwin":
		// Not hiddenInset: its toolbar makes the title bar as tall as AppKit's
		// (66 on macOS 26). Center the traffic lights in our own top bar.
		o.Vibrancy, o.TitleBarStyle = mygo.VibrancySidebar, mygo.TitleBarHidden
		o.Transparent = true
		o.TrafficLightPosition = &mygo.Point{X: trafficLightInset, Y: (theme.TopBarHeight - trafficLightSize) / 2}
	case "windows":
		o.Vibrancy, o.TitleBarHeight = mygo.VibrancyMica, 40
	}
	return o
}

// uiLocale keeps Godwit's Chinese fallback where MyGo distinguishes scripts.
func uiLocale(preference, system string) string {
	if preference == "" && strings.HasPrefix(system, "zh") && mygoi18n.Match([]string{system}, i18n.Catalog.Languages()) == "" {
		// shortcut: this fallback follows system changes on restart; remove when MyGo supports custom matching.
		return "zh-Hans"
	}
	return preference
}

// logOutput is where the GUI's log goes: RecentLog, for an error report, and
// stderr. A Windows release is a GUI program (-H windowsgui) without a
// stderr, so writing it fails; that must not stop io.MultiWriter before
// RecentLog, so RecentLog comes first and stderr's failures are dropped.
func logOutput(stderr io.Writer) io.Writer {
	return io.MultiWriter(app.RecentLog, bestEffort{stderr})
}

// bestEffort writes to w and reports success whatever w says.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	_, _ = b.w.Write(p) // a missing stderr is not the log failing
	return len(p), nil
}

// gui runs the desktop app on dataDir until it quits or ctx ends. On the
// default directory a second GUI focuses the first (singleInstance). It owns
// dataDir (ownerlock) before opening it; if another process does, it says so
// in an error dialog and exits 2.
func gui(ctx context.Context, dataDir string) int {
	log.SetOutput(logOutput(os.Stderr)) // and slog, while its default is unset; an error report carries the recent lines
	mygo.App.SetName("Godwit")
	if !packaged() { // the Microsoft Store updates the MSIX
		mygo.Use(updater.Plugin) // checks GitHub releases daily; a no-op in dev builds
	}
	if singleInstance(dataDir) && !mygo.App.RequestSingleInstanceLock() {
		return 0 // the first Godwit shows its window
	}
	defer context.AfterFunc(ctx, mygo.App.Quit)() // SIGINT / SIGTERM quit as the menu does
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := 0
	var release func() error
	var svc *core.Service
	var plat *platform
	var a *app.App
	var clicked []string                           // notification clicks before the App exists, as the one that launched it
	mygo.App.OnNotificationClick(func(id string) { // before Run, to get that one
		if a == nil {
			clicked = append(clicked, id)
			return
		}
		a.NotificationClicked(id) // it shows the window
	})
	// Compose pages reach only their own window's draft; bound before Run.
	pages := webui.NewCompose()
	// Embedded pages and their roles, by their *mygo.Page.
	roles := webui.NewPages(func(ctx context.Context) any { return mygo.CallerPage(ctx) })
	var windows *webui.Windows
	emit := func(t webui.Target, ev webui.WitEvent) {
		var err error
		if t.Page != 0 {
			if p, ok := roles.Key(t.Page); ok {
				err = webui.WitStream.EmitPage(p.(*mygo.Page), ev)
			}
		} else if w := mygo.WindowByID(t.Window); w != nil {
			err = webui.WitStream.Emit(w, ev)
		}
		if err != nil {
			log.Printf("wit event to %+v: %v", t, err)
		}
	}
	openCompose := func(id int64) { windows.Open(id) } // set below, before any page runs
	// Callbacks into the App run on its UI thread.
	onApp := func(f func(*app.App)) {
		if a != nil {
			a.OnUI(func() { f(a) }) // dropped once the App closed
		}
	}
	mygo.Bind(webui.NewComposePage(pages, webui.PageDeps{
		WinOf: func(ctx context.Context) (int, bool) {
			if w := mygo.CallerWindow(ctx); w != nil {
				return w.ID(), true
			}
			return 0, false
		},
		PickFiles:   pickFiles,
		Emit:        emit,
		OpenURL:     mygo.Shell.OpenExternal,
		OpenCompose: openCompose,
		OpenModels:  func() { onApp((*app.App).OpenModels) },
		OpenMessage: func(id int64) { onApp(func(a *app.App) { a.OpenMessage(id) }) },
	}))
	// The Wit page, embedded in the main window: its callbacks run on the
	// App's UI thread; its events go to its page only.
	var witView *mygo.WebView
	toWit := func(what string, emit func(*mygo.Page) error) {
		if witView == nil {
			return
		}
		if err := emit(witView.Page()); err != nil {
			log.Printf("%s to the Wit page: %v", what, err)
		}
	}
	wit := webui.NewWit(roles, webui.WitPageDeps{
		Emit: emit,
		Open: func(m webui.WitOpenMsg) {
			toWit("witOpen", func(p *mygo.Page) error { return webui.WitOpen.EmitPage(p, m) })
		},
		Ask: func(q string) {
			toWit("witAsk", func(p *mygo.Page) error { return webui.WitAsk.EmitPage(p, q) })
		},
		State: func(st webui.WitPageState) {
			toWit("witState", func(p *mygo.Page) error { return webui.WitState.EmitPage(p, st) })
		},
		OpenMessage:         func(id int64) { onApp(func(a *app.App) { a.OpenMessage(id) }) },
		OpenModels:          func() { onApp((*app.App).OpenModels) },
		Navigate:            func(dir int) { onApp(func(a *app.App) { a.Navigate(dir) }) },
		Search:              func() { onApp((*app.App).Search) },
		NewConversation:     func() { onApp((*app.App).NewConversation) },
		ConversationCreated: func(id, v int64) { onApp(func(a *app.App) { a.ConversationCreated(id, v) }) },
		OnLink:              func(u string) { onApp(func(a *app.App) { a.OpenLink(u) }) },
		OpenCompose:         func(id int64) { onApp(func(a *app.App) { a.OpenDraft(id) }) }, // a draft Wit made may be in the outbox
		EditRule: func(r core.Rule, conv, approval int64, ask string) {
			onApp(func(a *app.App) { a.EditRule(r, conv, approval, ask) })
		},
	})
	mygo.Bind(webui.NewWitPage(wit))
	// The reader pages, one per expanded message in the main window, each
	// bound to its message (platform.NewBodyView). Their callbacks run on
	// the App's UI thread (onBody), and only if the page is still open
	// there as one bound to the message (when one is named): a callback
	// queued before its page closed or rebound is dropped.
	onBody := func(k any, message int64, f func(*app.App, ui.NativeWebView)) {
		page, _ := k.(*mygo.Page)
		onApp(func(a *app.App) {
			if plat == nil {
				return
			}
			v, m, ok := plat.bodyView(page)
			if ok && (message == 0 || m == message) {
				f(a, v)
			}
		})
	}
	reader := webui.NewReader(roles, webui.ReaderDeps{
		Emit: func(k any, b webui.ReaderBody) {
			if err := webui.ReaderReload.EmitPage(k.(*mygo.Page), b); err != nil {
				log.Printf("a body to its reader page: %v", err)
			}
		},
		OnHeight: func(k any, m int64, px int) {
			onBody(k, m, func(a *app.App, v ui.NativeWebView) { a.BodyHeight(v, m, px) })
		},
		OnLink: func(k any, u string) { onBody(k, 0, func(a *app.App, v ui.NativeWebView) { a.BodyLink(v, u) }) },
		OnNavigate: func(k any, dir int) {
			onBody(k, 0, func(a *app.App, v ui.NativeWebView) { a.BodyNavigate(v, dir) })
		},
		OnSearch: func(k any) { onBody(k, 0, func(a *app.App, v ui.NativeWebView) { a.BodySearch(v) }) },
		OnSave:   func(k any) { onBody(k, 0, func(a *app.App, v ui.NativeWebView) { a.BodySave(v) }) },
		OnPrint:  func(k any) { onBody(k, 0, func(a *app.App, v ui.NativeWebView) { a.BodyPrint(v) }) },
		OnBuilt: func(k any, b webui.ReaderBody) {
			onBody(k, b.Message, func(a *app.App, v ui.NativeWebView) { a.BodyBuilt(v, b.Message, b.Cut, b.Remote) })
		},
		OnFailed: func(k any, m int64, err error) {
			onBody(k, m, func(a *app.App, v ui.NativeWebView) { a.BodyFailed(v, m, err) })
		},
		OnImagesFailed: func(k any, m int64, n int) {
			onBody(k, m, func(a *app.App, v ui.NativeWebView) { a.BodyImagesFailed(v, m, n) })
		},
	})
	mygo.Bind(webui.NewReaderPage(reader))
	// The image proxy: remote images of a body load from godwit-img: URLs.
	if err := mygo.Protocol.Handle(webui.ImageScheme, reader.ImageHandler()); err != nil {
		log.Printf("image proxy: %v", err)
	}
	// The signature page, embedded in the account settings
	// (platform.NewSignatureView).
	sig := webui.NewSignature(roles, pickImage)
	sig.OnHTML = func() { onApp((*app.App).SignatureHTML) }
	sig.OnLink = func(current string, answer func(webui.SignatureLinkAnswer)) {
		onApp(func(a *app.App) {
			a.SignatureLink(current, func(url string, ok bool) { answer(webui.SignatureLinkAnswer{URL: url, OK: ok}) })
		})
	}
	mygo.Bind(webui.NewSignaturePage(sig))
	var cas cascade
	windows = webui.NewWindows(pages, func(int64) webui.Window { return newComposeWindow(windows, &cas) }, askUnsaved)
	mygo.App.WhenReady(func() {
		lang := "en"
		if preferred := mygo.App.PreferredLanguages(); len(preferred) > 0 {
			lang = preferred[0]
		}
		var err error
		release, err = ownerlock.Acquire(dataDir)
		if err != nil {
			i18n.SetLanguage(lang)
			var msg string
			msg, code = lockFailure(err)
			mygo.Dialog.Error("Godwit", msg)
			mygo.App.Quit()
			return
		}
		reg, err := registrySource()
		if err == nil {
			svc, err = core.New(ctx, core.Options{DataDir: dataDir, AutoSync: true, LabelMail: true, SecurityMail: true, SystemLanguage: lang,
				Registry: reg, ModelsDev: ai.ModelsDevURL, AppVersion: app.Version, SystemTranslator: systemTranslator(), AppleIntelligence: appleIntelligence(),
				ConfirmWindow: true, ComposeOpen: windows.IsOpen, OpenURL: mygo.Shell.OpenExternal, GuardCoreML: guardCoreML(), PrintPDF: printPDF})
		}
		if err != nil {
			i18n.SetLanguage(lang)
			code = 1
			mygo.Dialog.Error("Godwit", openFailure(err))
			mygo.App.Quit()
			return
		}
		pages.SetService(svc)
		wit.SetService(svc)
		reader.SetService(svc)
		sig.SetService(svc)
		uiLang := "" // keep the database preference authoritative over MyGo's saved locale
		st, err := svc.GetSettings(ctx)
		if err != nil {
			log.Printf("reading settings: %v; the menus use the system language", err)
			st.ShowTrayIcon = true // the default: an unreadable preference must not take the icon away
		} else {
			uiLang = st.UILanguage
			// Before the window opens: its material and controls follow this, not the system's.
			mygo.Theme.SetSource(mygo.ThemeSource(st.Theme))
		}
		setLanguage := func(tag string) {
			if os.Getenv("MYGO_LOCALE") == "" {
				mygo.App.SetLocale(uiLocale(tag, lang))
			}
		}
		setLanguage(uiLang)
		i18n.SetLanguage(mygo.App.Locale())

		opts := windowOptions(runtime.GOOS)
		plat = &platform{dataDir: dataDir, svc: svc, windows: windows, wit: wit, roles: roles, reader: reader, sig: sig}
		var win *mygo.Window
		a = app.New(app.Deps{Svc: svc, Platform: plat, Settings: st, Post: func(f func()) { win.Update(f) },
			SystemLanguage: lang, SetLanguage: setLanguage,
			Vibrancy: opts.Vibrancy != mygo.VibrancyNone, Dev: mygo.IsDev()})
		plat.onAPIStop = a.LocalAPIStopped
		windows.OnChange = func() { a.OnUI(a.ComposeWindowsChanged) } // no window is open yet
		opts.Content = ui.View(a.View)
		win = mygo.NewWindow(opts)
		plat.win = win
		// The Wit page is registered before it loads (loadAfterSetup):
		// its first call finds its role.
		if v, err := win.NewWebView(mygo.WebViewOptions{Transparent: true}); err != nil {
			log.Printf("the Wit page: %v", err) // the Ask page shows no content
		} else if err := loadAfterSetup(func() {
			lockPage(v.Page())
			roles.Register(v.Page(), "wit")
		}, func() error { return v.Page().LoadURL(witURL(opts.Vibrancy != mygo.VibrancyNone)) }, func() {
			roles.Unregister(v.Page())
			v.Destroy()
		}); err != nil {
			log.Printf("the Wit page: %v", err)
		} else {
			witView = v
			a.SetWitPage(v, wit.SetConversation, wit.SetCreated)
			a.SetWitAsk(wit.Handoff)
		}
		win.OnFocus(func() { a.SetForeground(true) })
		win.OnBlur(func() { a.SetForeground(false) })
		mygo.App.OnSecondInstance(func([]string, string) { plat.ShowWindow() })
		mygo.App.OnActivate(func(visible bool) {
			if !visible {
				plat.ShowWindow()
			}
		})

		compose := func(*mygo.MenuItem, *mygo.Window) { a.Compose() }
		updateMenu := func() {
			// The Mail menu stands in for File, which on macOS holds Close (⌘W).
			mailMenu := []*mygo.MenuItem{{Label: i18n.T("mail.compose"), Accelerator: "CmdOrCtrl+N", Click: compose},
				mygo.Separator(), {Role: mygo.RoleClose}}
			appMenu := []*mygo.MenuItem{
				{Role: mygo.RoleAbout},
				mygo.Separator(),
				{Label: i18n.T("shell.nav.settings") + "…", Accelerator: "CmdOrCtrl+,", Click: func(*mygo.MenuItem, *mygo.Window) { plat.ShowWindow(); a.OpenSettings() }},
				mygo.Separator(),
				{Role: mygo.RoleServices},
				mygo.Separator(),
				{Role: mygo.RoleHide},
				{Role: mygo.RoleHideOthers},
				{Role: mygo.RoleUnhide},
				mygo.Separator(),
				{Role: mygo.RoleQuit},
			}
			if !packaged() { // the Store updates the MSIX
				appMenu = slices.Insert(appMenu, 3, updater.MenuItem())
			}
			mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
				{Label: "Godwit", Submenu: appMenu}, // the first menu is the app's, named by the bundle
				{Label: i18n.T("shell.nav.mail"), Submenu: mailMenu},
				{Role: mygo.RoleEditMenu},
				{Role: mygo.RoleWindowMenu},
			}))
			mygo.App.Dock.SetMenu(mygo.NewMenu([]*mygo.MenuItem{ // no-op off macOS
				{Label: i18n.T("tray.sync_all"), Click: func(*mygo.MenuItem, *mygo.Window) { a.SyncAll() }},
				{Label: i18n.T("mail.compose"), Click: compose},
			}))
		}
		updateMenu()
		trayMenu := func() *mygo.Menu {
			light := &mygo.MenuItem{Label: i18n.T("tray.light_background"), Type: mygo.MenuItemCheckbox, Checked: a.LightBackground(),
				Click: func(item *mygo.MenuItem, _ *mygo.Window) { a.SetLightBackground(item.IsChecked()) }}
			plat.trayLight = light
			items := []*mygo.MenuItem{
				{Label: i18n.T("tray.open"), Click: func(*mygo.MenuItem, *mygo.Window) { plat.ShowWindow() }},
				{Label: i18n.T("mail.compose"), Click: compose},
				{Label: i18n.T("tray.sync_all"), Click: func(*mygo.MenuItem, *mygo.Window) { a.SyncAll() }},
				light,
				mygo.Separator(),
				{Label: i18n.T("tray.hide"), Click: func(*mygo.MenuItem, *mygo.Window) { a.HideTrayIcon() }},
				mygo.Separator(),
				{Label: i18n.T("tray.quit"), Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
			}
			items = slices.Insert(items, 3, recentCodeMenu(plat.signInCodes, time.Now(), plat.signInCodeIcon, plat.useSignInCode)...)
			return mygo.NewMenu(items)
		}
		plat.updateTray = func() {
			if plat.tray != nil {
				plat.tray.SetMenu(trayMenu())
			}
		}

		plat.setTrayVisible = func(visible bool) {
			if visible == (plat.tray != nil) {
				return
			}
			if !visible {
				plat.ShowSignInCode(nil) // the banner is a flyout of the tray
				plat.tray.Destroy()
				plat.tray = nil
				return
			}
			if icon, err := trayIcon(trayBoxFor(runtime.GOOS)); err != nil {
				log.Print(err)
			} else if plat.tray, err = mygo.NewTray(mygo.TrayOptions{Icon: icon, IconIsTemplate: true, ToolTip: "Godwit",
				Menu: trayMenu()}); err != nil {
				log.Print(err) // no tray here (Linux without an indicator host)
			} else if runtime.GOOS == "windows" {
				plat.tray.OnClick(plat.ShowWindow) // Windows opens the app on a left click; the menu stays on the right
			}
		}
		plat.setTrayVisible(st.ShowTrayIcon)
		mygo.App.OnLocaleChanged(func(locale string) {
			i18n.SetLanguage(locale)
			updateMenu()
			if plat.tray != nil {
				plat.tray.SetMenu(trayMenu())
			}
		})

		// Closing the window hides it while the App keeps running in the
		// background; otherwise closing quits. Every quit goes through
		// quitHook: first the compose windows close, each flushing its page,
		// then nq waits for the main window's writes (a failed or slow one
		// asks first) and quits again. Hiding needs no save: nothing closes.
		nq := &nativeQuit{beforeClose: a.BeforeClose, close: a.Close, quit: func() { go quitAgain() }} // mygo's calls are goroutine-safe
		win.OnClose(func(e *mygo.CloseEvent) {
			if nq.flushed {
				return // the quit's last step closes it
			}
			e.PreventDefault()
			if a.KeepInBackground() {
				if closeMinimizes(runtime.GOOS, plat.tray != nil) {
					win.Minimize()
					return
				}
				win.Hide()
				return
			}
			mygo.App.Quit()
		})
		mygo.App.OnBeforeQuit(quitHook(windows, nq.onQuit, nq.reset))
		a.Start(ctx)
		a.SetForeground(win.IsFocused())
		for _, id := range clicked { // the App takes them once it has loaded; the last one wins
			a.NotificationClicked(id)
		}
		clicked = nil
	})
	// Before a relaunch starts the next Godwit, which needs dataDir's lock.
	mygo.App.OnQuit(func() {
		if plat != nil {
			plat.SetLocalAPI(0, false) // after BeforeClose, before the Service closes
		}
		if svc != nil {
			svc.Close()
		}
		if release != nil {
			release()
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
	return code
}

// quitApp quits again once the compose windows closed; tests swap it.
var quitApp = quitAgain

// relaunching is whether the quit under way came from mygo.App.Relaunch, as
// the updater's "Relaunch Now" does; quitHook records it.
var relaunching atomic.Bool

// quitAgain repeats the quit quitHook prevented: mygo forgets a prevented
// Relaunch, so a plain Quit would leave an updated Godwit closed.
func quitAgain() {
	if relaunching.Load() {
		mygo.App.Relaunch()
	} else {
		mygo.App.Quit()
	}
}

// calledByRelaunch reports whether mygo.App.Relaunch is on the stack.
// shortcut: mygo tells quit listeners nothing of a relaunch, so this matches its
// function name; use its flag once QuitEvent has one.
func calledByRelaunch() bool {
	pc := make([]uintptr, 64)
	frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
	for {
		f, more := frames.Next()
		if strings.HasPrefix(f.Function, "github.com/egoist/mygo.(*Application).Relaunch") {
			return true
		}
		if !more {
			return false
		}
	}
}

// quitHook is the one OnBeforeQuit listener: mygo runs every listener even
// after one prevented the quit. With compose windows open it prevents the
// quit, calls reset (the native flush must run after them, again if it ran)
// and closes them, each flushing its page; once all have gone it quits
// again, or stops when the user kept one. With none it runs native.
func quitHook(windows interface {
	Count() int
	CloseAll(func(bool))
}, native func(e *mygo.QuitEvent), reset func()) func(*mygo.QuitEvent) {
	return func(e *mygo.QuitEvent) {
		relaunching.Store(calledByRelaunch()) // each quit, ours repeated too, says again
		if windows.Count() == 0 {
			native(e)
			return
		}
		e.PreventDefault()
		reset()
		windows.CloseAll(func(ok bool) {
			if ok {
				quitApp()
			}
		})
	}
}

// nativeQuit is the main window's part of a quit: it waits for the main
// window's writes (beforeClose), then quits again; that pass closes the App.
// flushed holds for the current quit attempt only: reset forgets it and
// starts a new attempt (gen), so a flush still running from an earlier
// attempt neither marks this one flushed nor quits. Used on the UI thread only.
type nativeQuit struct {
	beforeClose func(proceed func()) // App.BeforeClose
	close       func()               // App.Close
	quit        func()
	flushed     bool
	gen         int
}

// onQuit prevents a quit not flushed yet and flushes; a flushed one closes
// the App and goes on.
func (n *nativeQuit) onQuit(e *mygo.QuitEvent) {
	if n.flushed {
		n.close()
		return
	}
	e.PreventDefault()
	gen := n.gen
	n.beforeClose(func() {
		if gen != n.gen {
			return // reset since: a later quit owns the outcome
		}
		n.flushed = true
		n.quit()
	})
}

// reset makes the next quit flush again and voids flushes still running.
func (n *nativeQuit) reset() { n.flushed = false; n.gen++ }

// openFailure is what the GUI tells the user when core.New fails: a class
// of reason, never err's text.
func openFailure(err error) string {
	reason := "app.open_failed.other"
	if errors.Is(err, fs.ErrPermission) {
		reason = "app.open_failed.permission"
	}
	return i18n.Tf("app.open_failed", i18n.T(reason))
}

// lockFailure is what the GUI tells the user when it cannot own the data
// directory, and its exit code. The message never carries err's text.
func lockFailure(err error) (msg string, code int) {
	if errors.Is(err, ownerlock.ErrAnotherOwner) {
		return i18n.T("app.another_owner"), 2
	}
	return i18n.T("app.lock_failed"), 1
}

// witURL is the Wit page; over the window's material, ?material tells it to
// tint it as the native root does rather than show it bare around its cards.
func witURL(material bool) string {
	if material {
		return "/wit.html?material"
	}
	return "/wit.html"
}
