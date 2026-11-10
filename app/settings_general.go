package app

import (
	"cmp"
	"context"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/icons"
	"github.com/godwit-email/godwit/internal/kit"
	"github.com/godwit-email/godwit/internal/sysproxy"
	"github.com/godwit-email/godwit/internal/theme"
)

// changeSettings shows st at once and saves it as a full snapshot; a save
// still waiting is replaced. A failure of the newest save reverts to the
// last confirmed settings.
func (a *App) changeSettings(st core.Settings) {
	old := a.settings
	a.settings = st
	a.settingsGen++
	a.settingsVer++
	ver := a.settingsVer
	a.settingsEffects(old, st)
	save := a.saveSettings
	var saved core.Settings
	a.writes.Submit("settings", a.settingsGen, func(ctx context.Context) (err error) {
		saved, err = save(ctx, st)
		return err
	}, func(gen uint64, err error) {
		a.refreshTasks() // saved or not, ask again: it may name other models
		if err == nil {
			a.settingsOK, a.settingsSavedVer = saved, ver
		}
		if gen != a.settingsGen {
			return
		}
		if err != nil {
			cur := a.settings
			a.settings = a.settingsOK
			a.settingsVer++
			a.settingsSavedVer = a.settingsVer
			a.settingsEffects(cur, a.settings)
			a.mail.reader.refreshAvatars()
			a.toastErr(err)
			return
		}
		a.settings = saved
		a.mail.reader.refreshAvatars()
	})
}

// settingsEffects tells the platform and the catalog what changed from old to st.
func (a *App) settingsEffects(old, st core.Settings) {
	if st.LightBackground != old.LightBackground {
		a.applyLightBackground(st)
	}
	if st.RemoteImages != old.RemoteImages {
		a.mail.reader.invalidateAvatars(true)
	}
	if st.Theme != old.Theme {
		a.plat.SetThemeSource(st.Theme)
	}
	if on := st.LaunchAtLogin; on != old.LaunchAtLogin {
		plat := a.plat
		a.writes.Chain("open-at-login", func(context.Context) error { return plat.SetOpenAtLogin(on) }, a.toastErr)
	}
	if st.UILanguage != old.UILanguage {
		a.setLanguage(st.UILanguage)
	}
	if st.ShowTrayIcon != old.ShowTrayIcon {
		a.plat.SetTrayVisible(st.ShowTrayIcon)
	}
	a.plat.SettingsChanged(st)
	if st.UnreadOnAppIcon != old.UnreadOnAppIcon || st.UnreadInMenuBar != old.UnreadInMenuBar || st.ShowTrayIcon != old.ShowTrayIcon {
		a.showUnread(a.inboxUnread)
	}
	a.syncLocalAPI()
}

// syncLocalAPI asks the platform to serve the Local API as a.settings say.
// Requests run in order and only the newest one's result counts: when it
// fails, the reason shows on the privacy page and the setting turns off.
func (a *App) syncLocalAPI() {
	on := a.settings.LocalAPI
	if on == a.localAPIWant {
		return
	}
	a.localAPIWant = on
	a.localAPIGen++
	gen := a.localAPIGen
	if on {
		a.localAPIErr = nil
	}
	plat := a.plat
	var path string
	a.writes.Chain("local-api", func(context.Context) (err error) {
		path, err = plat.SetLocalAPI(gen, on)
		return err
	}, func(err error) {
		if gen != a.localAPIGen { // a newer request runs after this one and decides
			return
		}
		a.localAPIPath = path
		if err != nil {
			a.localAPIWant = false // a failed request leaves the server off
			a.localAPIFailed(err)
		}
	})
}

// LocalAPIStopped tells the App that the Local API server that request gen
// asked for ended by itself with err. Safe from any goroutine.
func (a *App) LocalAPIStopped(gen uint64, err error) {
	a.onUI(func() {
		if gen != a.localAPIGen { // a newer request replaced or stopped that server
			return
		}
		a.localAPIPath = ""
		a.localAPIFailed(err)
	})
}

// localAPIFailed shows err on the privacy page and turns the setting off.
func (a *App) localAPIFailed(err error) {
	a.localAPIErr = err
	if a.settings.LocalAPI {
		st := a.settings
		st.LocalAPI = false
		a.changeSettings(st)
	}
}

func sameSettings(x, y core.Settings) bool {
	if !slices.Equal(x.NoTranslate, y.NoTranslate) {
		return false
	}
	x.NoTranslate, y.NoTranslate = nil, nil
	return reflect.DeepEqual(x, y)
}

// syncSettings saves what the controls changed since the last look, and
// then binds them to the App's settings again.
func (p *settingsPage) syncSettings() {
	if !sameSettings(p.st, p.shown) {
		st := p.st
		st.NoTranslate = slices.Clone(st.NoTranslate)
		p.a.changeSettings(st)
	}
	p.st = p.a.settings
	p.st.NoTranslate = slices.Clone(p.st.NoTranslate)
	p.shown = p.st
	p.shown.NoTranslate = slices.Clone(p.st.NoTranslate)
}

// languages are offered by their own names, for the UI, reading and
// no-translation settings alike, in the order of their tags.
var languages = []struct{ tag, name string }{
	{"de", "Deutsch"}, {"en", "English"}, {"es", "Español"}, {"fr", "Français"}, {"it", "Italiano"},
	{"ja", "日本語"}, {"ko", "한국어"}, {"nl", "Nederlands"}, {"pt", "Português"}, {"ru", "Русский"},
	{"tr", "Türkçe"}, {"vi", "Tiếng Việt"}, {"zh-Hans", "简体中文"}, {"zh-Hant", "繁體中文"},
}

func isZhTW(tag string) bool {
	t, _, _ := strings.Cut(strings.ToLower(strings.ReplaceAll(tag, "_", "-")), ".")
	return strings.HasPrefix(t, "zh-") && (t == "zh-tw" || strings.HasSuffix(t, "-tw"))
}

func (p *settingsPage) langLabel(l struct{ tag, name string }) string {
	if l.tag == "zh-Hant" && isZhTW(p.a.sysLang) {
		return "正體中文"
	}
	return l.name
}

func langName(tag string) string {
	if isZhTW(tag) {
		return "正體中文"
	}
	switch t := strings.ToLower(tag); {
	case strings.HasPrefix(t, "de"):
		return "Deutsch"
	case strings.HasPrefix(t, "zh-hant"), strings.HasPrefix(t, "zh-hk"), strings.HasPrefix(t, "zh-mo"):
		return "繁體中文"
	case strings.HasPrefix(t, "zh"):
		return "简体中文"
	case strings.HasPrefix(t, "ja"):
		return "日本語"
	case strings.HasPrefix(t, "ko"):
		return "한국어"
	case strings.HasPrefix(t, "fr"):
		return "Français"
	case strings.HasPrefix(t, "es"):
		return "Español"
	case strings.HasPrefix(t, "pt"):
		return "Português"
	case strings.HasPrefix(t, "it"):
		return "Italiano"
	case strings.HasPrefix(t, "ru"):
		return "Русский"
	case strings.HasPrefix(t, "nl"):
		return "Nederlands"
	case strings.HasPrefix(t, "tr"):
		return "Türkçe"
	case strings.HasPrefix(t, "vi"):
		return "Tiếng Việt"
	}
	return "English"
}

func sizeOptions(sizes []int, def, cur int) []kit.Option[int] {
	if !slices.Contains(sizes, cur) {
		sizes = append(slices.Clone(sizes), cur)
		slices.Sort(sizes)
	}
	opts := make([]kit.Option[int], len(sizes))
	for i, n := range sizes {
		key := "settings.px"
		if n == def {
			key = "settings.px_default"
		}
		opts[i] = kit.Option[int]{Value: n, Label: i18n.Tf(key, n)}
	}
	return opts
}

func (p *settingsPage) generalView(c *ui.Context) {
	p.syncSettings()
	defer p.syncSettings() // what handlers changed while building
	st := &p.st
	sel := func(label, desc string, control func() ui.Element) func() {
		return func() { settingRow(c, label, desc, func() { control() }) }
	}
	sw := func(on *bool, label, help string) func() {
		return func() { kit.SwitchRow(c, on, label, help) }
	}
	pageHeading(c, i18n.T("settings.general"), i18n.T("settings.general.sub"))
	settingsSection(c, i18n.T("settings.appearance"), "",
		func() {
			settingRow(c, i18n.T("settings.theme"), i18n.T("settings.theme.help"), func() {
				ui.Row(c).Gap(theme.ThemeTileGap).Children(func() {
					for _, th := range []string{"system", "light", "dark"} {
						p.themeTile(c, th)
					}
				})
			})
		},
		sel(i18n.T("settings.density"), "", func() ui.Element {
			l := i18n.T("settings.density")
			return settingSelect(c, l, &st.Density, []kit.Option[string]{
				{Value: "compact", Label: i18n.T("settings.density.compact")},
				{Value: "comfortable", Label: i18n.T("settings.density.comfortable")},
			})
		}),
		sel(i18n.T("settings.font_size"), "", func() ui.Element {
			return settingSelect(c, i18n.T("settings.font_size"), &st.FontSize, sizeOptions([]int{13, 14, 15, 16}, 14, st.FontSize))
		}),
		sel(i18n.T("settings.body_font_size"), i18n.T("settings.body_font_size.help"), func() ui.Element {
			opts := append([]kit.Option[int]{{Value: 0, Label: i18n.T("settings.body_font_size.ui")}}, sizeOptions([]int{16, 18}, 0, cmp.Or(st.BodyFontSize, 16))...)
			return settingSelect(c, i18n.T("settings.body_font_size"), &st.BodyFontSize, opts)
		}))
	settingsSection(c, i18n.T("settings.language"), "",
		sel(i18n.T("settings.ui_language"), "", func() ui.Element {
			opts := []kit.Option[string]{
				{Value: "", Label: i18n.Tf("settings.ui_language.system", langName(cmp.Or(p.a.sysLang, i18n.Language())))},
			}
			for _, l := range languages {
				opts = append(opts, kit.Option[string]{Value: l.tag, Label: p.langLabel(l)})
			}
			return settingSelect(c, i18n.T("settings.ui_language"), &st.UILanguage, opts)
		}))
	startup := []func(){
		sw(&st.LaunchAtLogin, i18n.T("settings.launch_at_login"), i18n.T("settings.launch_at_login.help")),
		sw(&st.KeepInBackground, i18n.T("settings.keep_in_background"), i18n.T("settings.keep_in_background.help")),
		sw(&st.LightBackground, i18n.T("settings.light_background"), i18n.T("settings.light_background.help")),
		sw(&st.UnreadOnAppIcon, i18n.T("settings.unread_on_app_icon"), ""),
		sw(&st.ShowTrayIcon, i18n.T("settings.show_tray_icon"), i18n.T("settings.show_tray_icon.help")),
	}
	if st.ShowTrayIcon {
		startup = append(startup, sw(&st.UnreadInMenuBar, i18n.T("settings.unread_in_menu_bar"), ""))
	}
	settingsSection(c, i18n.T("settings.startup"), "", startup...)
	settingsSection(c, i18n.T("settings.notifications"), "",
		sw(&st.NotifyNewMail, i18n.T("settings.notify_new_mail"), ""),
		sw(&st.NotifyPreview, i18n.T("settings.notify_preview"), i18n.T("settings.notify_preview.help")))
	settingsSection(c, i18n.T("settings.mail"), "",
		sw(&st.ThreadView, i18n.T("settings.thread_view"), i18n.T("settings.thread_view.help")),
		sw(&st.ThreadCollapse, i18n.T("settings.thread_collapse"), i18n.T("settings.thread_collapse.help")),
		func() {
			if !st.ThreadCollapse {
				return
			}
			t, th := theme.Of(c), c.Theme()
			settingRow(c, i18n.T("settings.thread_collapse_threshold"), i18n.T("settings.thread_collapse_threshold.help"), func() {
				n := float64(st.ThreadCollapseThreshold)
				ui.Row(c).Width(theme.UsageSelectMin).Gap(th.Space(3)).AlignItems(ui.Center).Children(func() {
					if ui.Slider(c, &n, core.MinThreadCollapse, core.MaxThreadCollapse).Step(1).Grow(1).Shrink(1).Label(i18n.T("settings.thread_collapse_threshold")).Changed() {
						st.ThreadCollapseThreshold = int(math.Round(n))
					}
					kit.Text(c, strconv.Itoa(st.ThreadCollapseThreshold), t.Text.Sm).Width(th.Space(10)).TextAlign(ui.End).TextColor(t.Muted)
				})
			})
		})
	p.proxySection(c)
}

// proxyDraft is 网络代理 as shown: the custom address saves once it is
// usable, so 自定义 waits for one, and it outlasts other modes.
type proxyDraft struct {
	mode, addr         string // addr is scheme://host:port
	bypass             string // 不使用代理的主机, comma-separated
	addrErr, bypassErr string // why the fields can't be saved yet
	from               string // the settings the draft last matched (proxyKey)
	pass, sent         string // the password typed, never the stored one; and what of it went to the settings
}

func (p *settingsPage) proxySection(c *ui.Context) {
	st, d, t := &p.st, &p.proxy, theme.Of(c)
	if key := proxyKey(st); key != d.from {
		d.from, d.mode, d.addr = key, cmp.Or(st.ProxyMode, core.ProxySystem), st.ProxyURL
		d.bypass = strings.Join(st.ProxyBypass, ", ")
	}
	source := "" // where 跟随系统 finds the proxy
	if d.mode == core.ProxySystem {
		source = i18n.T("settings.proxy.system.settings")
		if sysproxy.FromEnvironment() {
			source = i18n.T("settings.proxy.system.env")
		}
	}
	input := func(label, msg string, v *string, opts kit.InputOpts) func() {
		return func() {
			settingRow(c, label, msg, func() {
				kit.Input(c, v, opts).Width(theme.SettingsInputWidth).Radius(t.Radius.Lg).Label(label)
			})
		}
	}
	rows := []func(){func() {
		settingRow(c, i18n.T("settings.proxy.mode"), source, func() {
			settingSelect(c, i18n.T("settings.proxy.mode"), &d.mode, []kit.Option[string]{
				{Value: core.ProxySystem, Label: i18n.T("settings.proxy.system")},
				{Value: core.ProxyCustom, Label: i18n.T("settings.proxy.custom")},
				{Value: core.ProxyOff, Label: i18n.T("settings.proxy.off")},
			})
		})
	}}
	if d.mode == core.ProxyCustom {
		rows = append(rows,
			input(i18n.T("settings.proxy.url"), cmp.Or(d.addrErr, i18n.T("settings.proxy.url.help")), &d.addr,
				kit.InputOpts{Invalid: d.addrErr != "", Placeholder: "socks5://127.0.0.1:1080"}),
			input(i18n.T("settings.proxy.user"), "", &st.ProxyUser, kit.InputOpts{Placeholder: i18n.T("settings.proxy.optional")}),
			func() {
				ph := i18n.T("settings.proxy.optional")
				if st.ProxyPasswordSet {
					ph = i18n.T("settings.proxy.password.kept")
				}
				settingRow(c, i18n.T("settings.proxy.password"), "", func() {
					ui.Row(c).Gap(c.Theme().Space(3)).AlignItems(ui.Center).Children(func() {
						kit.Input(c, &d.pass, kit.InputOpts{Password: true, Placeholder: ph}).Width(theme.SettingsInputWidth).Radius(t.Radius.Lg).Label(i18n.T("settings.proxy.password"))
						if st.ProxyPasswordSet && d.pass == "" { // a saved one shows only as the placeholder, so erasing can't reach it
							settingButton(c, i18n.T("settings.proxy.password.clear"), kit.Outline, false).OnClick(func() { st.ProxyPasswordSet = false })
						}
					})
				})
			},
			input(i18n.T("settings.proxy.bypass"), d.bypassErr, &d.bypass,
				kit.InputOpts{Invalid: d.bypassErr != "", Placeholder: "*.corp.example, .lan, 10.0.0.0/8"}))
	}
	// a typed password saves; erasing it removes the stored one
	if d.pass != d.sent {
		st.ProxyPassword, st.ProxyPasswordSet, d.sent = d.pass, d.pass != "", d.pass
	}
	settingsSection(c, i18n.T("settings.proxy"), i18n.T("settings.proxy.foot"), rows...)

	// what the controls changed goes to the settings once it can be saved
	d.addrErr, d.bypassErr = "", ""
	bypass := strings.FieldsFunc(d.bypass, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(bypass) == 0 {
		bypass = nil // as an empty list loads
	}
	addr := strings.TrimSuffix(strings.TrimSpace(d.addr), "/")
	u, err := core.ParseProxyURL(addr)
	switch {
	case d.mode != core.ProxyCustom:
		if d.mode != cmp.Or(st.ProxyMode, core.ProxySystem) {
			st.ProxyMode = d.mode
		}
	case addr == "": // nothing to save yet; the help says what goes here
	case err != nil:
		d.addrErr = i18n.T("settings.proxy.url.invalid")
	case len(bypass) > core.MaxProxyBypass:
		d.bypassErr = i18n.Tf("settings.proxy.bypass.too_many", core.MaxProxyBypass)
	default:
		st.ProxyMode, st.ProxyURL, st.ProxyBypass = core.ProxyCustom, u.String(), bypass
	}
	d.from = proxyKey(st)
}

// proxyKey is what of st proxyDraft follows (proxyDraft.from).
func proxyKey(st *core.Settings) string {
	return st.ProxyMode + " " + st.ProxyURL + " " + strings.Join(st.ProxyBypass, ",")
}

// aiFeaturesView is the Intelligence page's 功能 tab: the reader's AI features, and
// the languages and translation they use.
func (p *settingsPage) aiFeaturesView(c *ui.Context) {
	p.syncSettings()
	defer p.syncSettings() // what handlers changed while building
	st := &p.st
	sel := func(label, desc string, control func() ui.Element) func() {
		return func() { settingRow(c, label, desc, func() { control() }) }
	}
	sw := func(on *bool, label, help string) func() {
		return func() { kit.SwitchRow(c, on, label, help) }
	}
	settingsSection(c, i18n.T("settings.ai"), "",
		sw(&st.Summary, i18n.T("settings.summary"), i18n.T("settings.summary.help")),
		sw(&st.QuickReplies, i18n.T("settings.quick_replies"), i18n.T("settings.quick_replies.help")),
		sw(&st.RuleAI, i18n.T("settings.rule_ai"), i18n.T("settings.rule_ai.help")))
	settingsSection(c, i18n.T("settings.language"), "",
		sel(i18n.T("settings.reading_language"), i18n.T("settings.reading_language.help"), func() ui.Element {
			opts := []kit.Option[string]{{Value: "", Label: i18n.T("settings.reading_language.ui")}}
			for _, l := range languages {
				opts = append(opts, kit.Option[string]{Value: l.tag, Label: p.langLabel(l)})
			}
			return settingSelect(c, i18n.T("settings.reading_language"), &st.ReadingLanguage, opts)
		}),
		sel(i18n.T("settings.reply_language"), i18n.T("settings.reply_language.help"), func() ui.Element {
			return settingSelect(c, i18n.T("settings.reply_language"), &st.ReplyLanguage, []kit.Option[string]{
				{Value: "match", Label: i18n.T("settings.reply_language.match")},
				{Value: "reading", Label: i18n.T("settings.reply_language.reading")},
			})
		}),
		sel(i18n.T("settings.translator"), i18n.T("settings.translator.help"), func() ui.Element {
			return settingSelect(c, i18n.T("settings.translator"), &st.Translator, p.translatorOptions(st.Translator))
		}),
		func() {
			settingRow(c, i18n.T("settings.no_translate"), i18n.T("settings.no_translate.help"), func() { p.noTranslate(c) })
		})
}

// themeTile is a small picture of the window in a theme: rail, sidebar and
// content; "system" is the light window on its left half and the dark one on
// its right. The name is only its tooltip and label.
func (p *settingsPage) themeTile(c *ui.Context, th string) {
	t := theme.Of(c)
	on := p.st.Theme == th
	light, dark := theme.Light(), theme.Dark()
	if th == "dark" {
		light = dark
	}
	// fill paints a part spanning x0 to x0+w of the picture: in "system", light
	// left of the picture's middle and dark right of it, each part cut where it crosses.
	fill := func(e ui.Element, x0, w float32, l, d ui.Color) ui.Element {
		f := (theme.ThemeTileWidth/2 - x0) / w
		switch {
		case th != "system" || f >= 1:
			return e.Background(l)
		case f <= 0:
			return e.Background(d)
		}
		return e.LinearGradient(ui.LinearGradient{From: l, To: d, Angle: 90, Start: f, End: f})
	}
	label := i18n.T("settings.theme." + th)
	b := kit.Pressable(c, kit.PressableOpts{Bare: true, OnClick: func() { p.st.Theme = th }}).Label(label).Role(ui.RoleRadio).Checked(on).
		Size(theme.ThemeTileWidth, theme.ThemeTileHeight).Radius(t.Radius.Md).Clip().Border(theme.Stroke, t.Border)
	fill(b, 0, theme.ThemeTileWidth, light.Window, dark.Window)
	if on {
		b.Border(theme.SelectedBorder, t.Primary)
	}
	const content = theme.TileRail + theme.TileSide                  // the content's left edge
	const inner = theme.ThemeTileWidth - content - 2*theme.TileInset // its lines' full width
	b.AlignItems(ui.Stretch).Children(func() {
		ui.Box(c).Width(theme.TileRail).Shrink(0)
		fill(ui.Box(c).Width(theme.TileSide).Shrink(0).Margin(theme.TileInset, 0, 0, 0).Radius(t.Radius.Md, 0, 0, 0), theme.TileRail, theme.TileSide, light.Sidebar, dark.Sidebar)
		col := ui.Column(c).Grow(1).Margin(theme.TileInset, 0, 0, 0).Padding(theme.TileInset).Gap(theme.TileGap)
		fill(col, content, theme.ThemeTileWidth-content, light.Background, dark.Background).Children(func() {
			for _, pct := range []float32{70, 45} {
				fill(ui.Box(c).Height(theme.TileLine).WidthPercent(pct).Radius(t.Radius.Xs), content+theme.TileInset, inner*pct/100, light.Border, dark.Border)
			}
		})
	})
	kit.TooltipLabelled(c, b, label)
}

// noTranslate picks the languages that need no translation.
func (p *settingsPage) noTranslate(c *ui.Context) {
	th := c.Theme()
	var names []string
	for _, l := range languages {
		if slices.Contains(p.st.NoTranslate, l.tag) {
			names = append(names, p.langLabel(l))
		}
	}
	label := strings.Join(names, i18n.T("mail.list_sep"))
	if label == "" {
		label = i18n.T("settings.no_translate.none")
	}
	t := theme.Of(c)
	anchor := kit.Pressable(c, kit.PressableOpts{Bare: true, OnClick: func() { p.noTrans = !p.noTrans }}).Label(i18n.T("settings.no_translate")).
		Height(th.Space(8)).PaddingX(th.Space(2.5)).Gap(th.Space(2)).Radius(t.Radius.Lg).Border(theme.Stroke, t.Input).Background(t.Background).
		MinWidth(theme.SettingsSelectMin).MaxWidth(theme.SettingsSelectMax)
	anchor.Children(func() {
		ui.Text(c, label).Grow(1)
		ui.Icon(c, icons.Get("chevron-down")).Size(theme.Icon, theme.Icon).TextColor(t.Muted)
	})
	kit.Popover(c, anchor, &p.noTrans, func() {
		ui.Column(c).Gap(th.Space(2)).Children(func() {
			for _, l := range languages {
				on := slices.Contains(p.st.NoTranslate, l.tag)
				if kit.Checkbox(c.Key(l.tag), &on, p.langLabel(l)).Changed() {
					if i := slices.Index(p.st.NoTranslate, l.tag); i >= 0 {
						p.st.NoTranslate = slices.Delete(p.st.NoTranslate, i, i+1)
					} else {
						p.st.NoTranslate = append(p.st.NoTranslate, l.tag)
					}
				}
			}
		})
	})
}

// translatorOptions are the 翻译方式 choices: the model, then each plugin
// that can translate (marked when off), and the chosen one if it is gone.
func (p *settingsPage) translatorOptions(cur string) []kit.Option[string] {
	opts := []kit.Option[string]{{Value: "", Label: i18n.T("settings.translator.model")}}
	found := cur == ""
	for _, tr := range p.translators {
		if tr.ID == "" {
			continue
		}
		label := tr.Name
		if tr.Builtin {
			label = tr.Title.For(i18n.Language())
		}
		for _, en := range p.ext.reg.Entries {
			if en.Name == tr.Name {
				label = entryTitle(en)
				break
			}
		}
		if !tr.Running {
			label = i18n.Tf("settings.translator.off", label)
		}
		opts = append(opts, kit.Option[string]{Value: tr.ID, Label: label})
		found = found || tr.ID == cur
	}
	if !found {
		opts = append(opts, kit.Option[string]{Value: cur, Label: i18n.Tf("settings.translator.gone", strings.TrimPrefix(cur, "plugin/"))})
	}
	return opts
}
