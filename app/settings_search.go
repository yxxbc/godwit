package app

import (
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/icons"
	"github.com/godwit-email/godwit/internal/kit"
	"github.com/godwit-email/godwit/internal/theme"
)

// settingsIndex is the sidebar search's hand-kept index: the catalog keys of
// each page's titles and rows ("account" is every account page).
// TestSettingsIndexCoversRows fails when a row is missing.
var settingsIndex = map[string][]string{
	"account":    {"settings.accounts", "settings.account.basic", "settings.account.name", "settings.account.name.help", "settings.account.alias", "settings.account.icon", "settings.account.icon.help", "settings.account.icon.choose", "settings.account.make_default", "settings.account.make_default.help", "settings.account.default", "settings.account.status.ok", "settings.account.signature", "settings.account.signature.default", "settings.account.signature.default.help", "settings.account.signature.none", "settings.account.signature.preview", "settings.account.signature.foot", "settings.account.signature.where", "settings.account.ai", "settings.account.ai.help", "settings.account.sync", "settings.account.sync.status", "settings.account.sync_now", "settings.account.sync.range", "settings.account.sync.2000", "settings.account.label_keywords", "settings.account.label_keywords.help", "settings.account.usage", "settings.account.usage.help", "settings.account.clear_cache", "settings.account.mailbox", "settings.account.mailbox.help", "settings.account.mailbox.none", "settings.account.mailbox.drafts.help", "settings.account.mailbox.trash.help", "settings.account.mailbox.archive.help", "mail.folder.drafts", "mail.folder.sent", "mail.folder.junk", "mail.folder.trash", "mail.folder.archive", "settings.account.purge.0", "settings.account.purge.junk", "settings.account.purge.junk.help", "settings.account.purge.trash", "settings.account.purge.trash.help", "settings.ext.skill.on.draft", "settings.account.login", "settings.account.login.method", "settings.account.servers", "settings.account.servers.help", "settings.account.edit_servers", "settings.ext.mcp.edit", "settings.account.remove", "settings.account.remove.button", "settings.account.remove.row"},
	"general":    {"settings.general", "settings.general.sub", "settings.appearance", "settings.theme", "settings.theme.help", "settings.theme.system", "settings.theme.light", "settings.theme.dark", "settings.density", "settings.density.compact", "settings.density.comfortable", "settings.font_size", "settings.body_font_size", "settings.body_font_size.help", "settings.body_font_size.ui", "settings.language", "settings.ui_language", "settings.ui_language.system", "settings.startup", "settings.launch_at_login", "settings.launch_at_login.help", "settings.keep_in_background", "settings.keep_in_background.help", "settings.light_background", "settings.light_background.help", "settings.unread_in_menu_bar", "settings.unread_on_app_icon", "settings.show_tray_icon", "settings.show_tray_icon.help", "settings.notifications", "settings.notify_new_mail", "settings.notify_preview", "settings.notify_preview.help", "settings.mail", "settings.thread_view", "settings.thread_view.help", "settings.thread_collapse", "settings.thread_collapse.help", "settings.thread_collapse_threshold", "settings.thread_collapse_threshold.help", "settings.proxy", "settings.proxy.foot", "settings.proxy.mode", "settings.proxy.system", "settings.proxy.off", "settings.proxy.system.settings", "settings.proxy.system.env", "settings.proxy.custom", "settings.proxy.url", "settings.proxy.url.help", "settings.proxy.bypass", "settings.proxy.user", "settings.proxy.password", "settings.proxy.password.clear"},
	"signatures": {"settings.signatures", "settings.signatures.sub", "settings.signatures.new", "settings.signatures.foot", "settings.signatures.options", "settings.signatures.delimiter", "settings.signatures.delimiter.help", "settings.signatures.empty.title", "settings.signatures.empty.desc", "settings.signatures.unused", "settings.account.basic", "settings.signature.name", "settings.signature.name.help", "settings.signature.content", "settings.account.signature.help", "settings.account.signature.html.help", "settings.signature.default", "settings.signature.default.foot", "settings.signature.using", "settings.signature.uses_none", "settings.signature.delete", "settings.signature.delete.row", "settings.signature.delete.unused"},
	"models":     slices.Concat(aiTabs...),
	"labels":     {"settings.labels", "settings.labels.sub", "settings.labels.new", "settings.labels.list", "settings.labels.list.foot", "settings.labels.builtin", "settings.labels.no_desc", "settings.labels.display", "settings.labels.display.style", "settings.labels.display.style.help", "settings.labels.display.both", "settings.labels.display.icon", "settings.labels.display.text", "settings.labels.auto", "settings.labels.auto.on", "settings.labels.auto.on.help", "settings.labels.model", "settings.labels.model.help", "settings.labels.model.value", "settings.labels.model.go", "settings.labels.rescan", "settings.labels.rescan.sub", "settings.labels.rescan.range", "settings.labels.rescan.all_range", "settings.labels.rescan.mode", "settings.labels.rescan.missing", "settings.labels.rescan.missing.help", "settings.labels.rescan.redo", "settings.labels.rescan.redo.help", "settings.labels.rescan.start", "settings.labels.rescan.stop", "settings.labels.model.none", "settings.labels.rescan.no_model", "settings.notifications"},
	"rules":      {"settings.rules", "settings.rules.sub", "settings.rules.new", "settings.rules.empty", "settings.rules.empty.desc", "settings.rules.empty.wit", "settings.rules.list", "settings.rules.list.foot", "settings.rules.empty_trash", "settings.rules.templates", "settings.rules.templates.foot", "settings.rules.template.bill", "settings.rules.template.bill.desc", "settings.rules.template.newsletter", "settings.rules.template.newsletter.desc", "settings.rules.template.suspicious", "settings.rules.template.suspicious.desc", "settings.rules.template.forward", "settings.rules.template.forward.desc", "settings.rules.template.contract", "settings.rules.template.contract.desc", "settings.rules.template.reply", "settings.rules.template.reply.desc"},
	"extensions": {"settings.extensions", "settings.ext.sub", "settings.ext.install", "settings.ext.install.hint", "settings.ext.install.choose", "settings.ext.no_send", "settings.ext.browse", "settings.ext.browse.hint", "settings.ext.directory", "settings.ext.plugins.empty", "settings.ext.tab.skills", "settings.ext.tab.plugins", "settings.ext.sec.skills", "settings.ext.sec.installed", "settings.ext.sec.add"},
	"privacy":    slices.Concat(privacyTabs...),
	"about":      {"settings.about", "settings.about.build", "settings.about.updates.check", "settings.about.upgrade", "settings.about.links", "settings.about.site", "settings.about.source", "settings.about.star", "settings.about.changelog", "settings.about.feedback", "settings.about.license"},
}

// aiTabs and privacyTabs are the AI and 隐私与安全 pages' index, a tab's
// keys each (the page's title and its tabs' names go with the first); a
// search opens the page on the first tab that matches (tabFor).
var aiTabs = [][]string{
	{"settings.models", "settings.models.sub", "settings.models.add", "settings.models.usage", "settings.models.threshold", "settings.models.threshold.help", "settings.models.threshold.binary", "settings.models.use.gen", "settings.models.use.gen.desc", "settings.models.use.small", "settings.models.use.small.desc", "settings.models.use.judge", "settings.models.use.judge.desc", "settings.models.services", "settings.models.foot", "settings.models.empty.title", "settings.models.empty.desc", "settings.models.local", "settings.models.files", "settings.models.files.foot", "settings.models.test", "settings.models.api_key", "settings.models.base_url", "settings.ai.tab.models", "settings.ai.tab.features"},
	{"settings.ai", "settings.summary", "settings.summary.help", "settings.quick_replies", "settings.quick_replies.help", "settings.rule_ai", "settings.rule_ai.help", "settings.language", "settings.reading_language", "settings.reading_language.help", "settings.reading_language.ui", "settings.reply_language", "settings.reply_language.help", "settings.reply_language.match", "settings.reply_language.reading", "settings.translator", "settings.translator.help", "settings.translator.model", "settings.no_translate", "settings.no_translate.help", "settings.no_translate.none"},
	{"settings.ext.tab.mcp", "settings.ext.mcp.add", "settings.ext.mcp.empty"},
	{"settings.ai.tab.apps", "settings.local_api", "settings.local_api.foot", "settings.local_api.connect", "settings.local_api.connect.help", "settings.local_api.path", "settings.local_api.on", "settings.local_api.help", "settings.local_api.send", "settings.local_api.send.help", "settings.local_api.send.off", "settings.local_api.send.confirm", "settings.local_api.send.direct"},
}

var privacyTabs = [][]string{
	{"settings.privacy", "settings.privacy.sub", "settings.keychain", "settings.keychain.checking", "settings.keychain.ok", "settings.masking", "settings.masking.on", "settings.masking.on.help", "settings.masking.foot", "settings.masking.recognition_model", "settings.masking.recognition_model.help", "settings.masking.recognition_model.missing", "settings.masking.cat.secrets", "settings.masking.cat.secrets.help", "settings.masking.cat.ids", "settings.masking.cat.ids.help", "settings.masking.cat.codes", "settings.masking.cat.codes.help", "settings.masking.cat.phones", "settings.masking.cat.phones.help", "settings.masking.cat.emails", "settings.masking.cat.emails.help", "settings.masking.cat.names", "settings.masking.cat.names.help", "settings.masking.cat.addresses", "settings.masking.cat.addresses.help", "settings.masking.cat.accounts", "settings.masking.cat.accounts.help", "settings.masking.action", "settings.masking.action.mask", "settings.masking.action.warn", "settings.masking.action.block", "settings.masking.custom", "settings.masking.custom.foot", "settings.masking.words", "settings.masking.words.help", "settings.masking.words.case", "settings.masking.words.whole", "settings.masking.words.whole.help", "settings.masking.rules", "settings.masking.rules.help", "settings.masking.allow", "settings.masking.allow.help", "settings.masking.save", "settings.masking.preview", "settings.masking.preview.help", "settings.masking.log", "settings.masking.log.foot", "settings.masking.log.empty", "settings.masking.log.open"},
	{"settings.security", "settings.security.warnings", "settings.security.warnings.help", "settings.security.verified", "settings.security.verified.help", "settings.security.ai", "settings.security.ai.help", "settings.security.rescan", "settings.security.rescan.sub", "settings.security.rescan.range", "settings.security.rescan.all_range", "settings.security.rescan.mode", "settings.security.rescan.missing", "settings.security.rescan.missing.help", "settings.security.rescan.redo", "settings.security.rescan.redo.help", "settings.security.rescan.start", "settings.security.rescan.busy", "settings.security.rescan.checking", "settings.security.rescan.stop"},
	{"settings.remote_images", "settings.remote_images.sub", "settings.remote_images.mode", "settings.remote_images.mode.help", "settings.remote_images.auto", "settings.remote_images.trusted", "settings.remote_images.ask", "settings.remote_images.cache", "settings.remote_images.cache.help", "settings.remote_images.clear", "settings.trusted", "settings.trusted.sub", "settings.trusted.none"},
	{"settings.blocked", "settings.blocked.sub", "settings.blocked.none", "settings.blocked.remove"},
}

// tabFor is the first of tabs whose keys hold the lowercased query q;
// cur when none does.
func tabFor(tabs [][]string, q string, cur int) int {
	for i, keys := range tabs {
		if hasQuery(keys, q) {
			return i
		}
	}
	return cur
}

// searchHit is what the sidebar shows for a query: the app pages that match,
// and whether the accounts group does.
type searchHit struct {
	apps     []string
	accounts bool
	email    string // set when only accounts' emails matched: the sidebar lists those
}

func hasQuery(keys []string, q string) bool {
	for _, k := range keys {
		if strings.Contains(strings.ToLower(i18n.T(k)), q) {
			return true
		}
	}
	return false
}

// accountOptions are the labels of the account page's selects, formatted
// (最近 3 个月), which the index's keys cannot match.
func accountOptions() []string {
	var out []string
	for _, o := range rangeOptions() {
		out = append(out, o.Label)
	}
	for _, o := range purgeOptions() {
		out = append(out, o.Label)
	}
	return out
}

func (p *settingsPage) matching(query string) (m searchHit) {
	q := strings.ToLower(strings.TrimSpace(query))
	m.accounts = q == "" || hasQuery(settingsIndex["account"], q) ||
		slices.ContainsFunc(accountOptions(), func(l string) bool { return strings.Contains(strings.ToLower(l), q) })
	if !m.accounts && p.accountByEmail(q) != nil {
		m.accounts, m.email = true, q
	}
	for _, sec := range appSections {
		if q == "" || hasQuery(settingsIndex[sec], q) {
			m.apps = append(m.apps, sec)
		}
	}
	return
}

// accountByEmail is the first account whose email holds q, or nil.
func (p *settingsPage) accountByEmail(q string) *core.Account {
	if q == "" {
		return nil
	}
	for i, acc := range p.a.accounts {
		if accountMatches(acc, q) {
			return &p.a.accounts[i]
		}
	}
	return nil
}

// accountMatches reports whether a lowercased query is in the account's email or its sidebar name.
func accountMatches(acc core.Account, q string) bool {
	return strings.Contains(strings.ToLower(acc.Email), q) || strings.Contains(strings.ToLower(acc.Alias), q)
}

// searchField is the sidebar's 搜索设置 field; Enter opens the first match.
func (p *settingsPage) searchField(c *ui.Context) {
	t, th := theme.Of(c), c.Theme()
	box := kit.InputGroup(c, kit.InputOpts{Flat: true}).FillWidth().Gap(th.Space(2)).PaddingX(th.Space(2.5)).Margin(0, 0, th.Space(1.5), theme.RingWidth).Background(t.HoverTint)
	box.Children(func() {
		ui.Icon(c, icons.Get("search")).Size(theme.Icon, theme.Icon).TextColor(t.Muted)
		in := kit.Input(c, &p.query, kit.InputOpts{Placeholder: i18n.T("settings.search"), Bare: true}).FocusRing(false).Label(i18n.T("settings.search")).Grow(1)
		if q := strings.ToLower(strings.TrimSpace(p.query)); q != p.searched { // a search picks the tabs it finds, on the page open too
			p.searched = q
			if q != "" {
				p.aiTab = tabFor(aiTabs, q, p.aiTab)
				p.privacyTab = tabFor(privacyTabs, q, p.privacyTab)
			}
		}
		if in.Submitted() {
			m := p.matching(p.query)
			switch {
			case m.accounts && len(p.a.accounts) > 0:
				acc := &p.a.accounts[0]
				if m.email != "" {
					acc = p.accountByEmail(m.email)
				}
				p.a.navigate(Route{Page: PageSettings, Settings: accountRoute(acc.ID)})
			case len(m.apps) > 0:
				p.a.navigate(Route{Page: PageSettings, Settings: m.apps[0]})
			}
		}
	})
}
