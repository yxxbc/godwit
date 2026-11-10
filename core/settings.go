package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/core/ai"
	"github.com/godwit-email/godwit/core/ai/mask"
	"github.com/godwit-email/godwit/core/events"
	gomail "github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/skills"
	"github.com/godwit-email/godwit/core/store"
)

// Settings are the app-wide preferences, stored as one JSON row.
// ReplyLanguage is "match" (the mail's language) or "reading".
type Settings struct {
	Theme, Density                                                 string
	FontSize, BodyFontSize                                         int // BodyFontSize 0 follows FontSize
	UILanguage, ReadingLanguage, ReplyLanguage                     string
	NoTranslate                                                    []string
	LaunchAtLogin, KeepInBackground, ShowTrayIcon, UnreadInMenuBar bool
	UnreadOnAppIcon                                                bool
	// LightBackground syncs the inbox only while the window is in the
	// background; the other folders catch up when it comes back.
	LightBackground                   bool
	NotifyNewMail, NotifyPreview      bool
	QuickReplies, Summary, ThreadView bool
	ThreadCollapse                    bool // collapse runs of folded messages into an expandable card
	ThreadCollapseThreshold           int  // a longer thread keeps its newest this many messages in sight and collapses the older; MinThreadCollapse to MaxThreadCollapse
	DefaultModel, SmallModel          string
	// Translator is the reader's translation channel: "" the model, or
	// "plugin/<name>" a plugin declaring translate (ListTranslators).
	Translator string
	// JudgeService is the slug of the service whose judge answers typed
	// questions; "" (or one gone or with judge off): the DefaultModel does.
	JudgeService string
	// JudgeThreshold is the minimum predicate probability treated as yes by
	// app features, from MinJudgeThreshold to MaxJudgeThreshold. Raw judge
	// answers keep their probabilities.
	JudgeThreshold float64
	LocalAPI       bool // the GUI serves the Local API; off by default
	// MCPSend is what godwit mcp's send_draft does (通过 MCP 发送):
	// MCPSendOff, MCPSendConfirm (the default; "" too) or MCPSendDirect.
	MCPSend string
	// SignatureDelimiter puts the RFC 3676 "-- " line before a new draft's
	// signature, which clients such as Mutt and Thunderbird recognize and
	// fold; off by default.
	SignatureDelimiter bool
	// AutoLabel lets the classifier label new Inbox mail; on by default.
	// A rescan runs either way.
	AutoLabel bool
	// Masking is 脱敏 (docs/adr/0025-mask-at-cloud-egress.md): keys, ID and
	// card numbers go to a model off this device as placeholders. On by
	// default, for settings stored before it too.
	Masking bool
	// MaskCategories say which categories masking finds, and what is done
	// with each one's values.
	MaskCategories MaskCategories
	// MaskWords are the user's own words to mask (自定义词), as TERM: in either
	// letter case unless MaskWordsCase, inside longer words unless MaskWordsWhole.
	MaskWords                     []string
	MaskWordsCase, MaskWordsWhole bool
	// MaskRules are the user's patterns (自定义正则), each masking as its kind;
	// at most mask.MaxRules, checked as they are saved.
	MaskRules []mask.Rule
	// MaskAllow are the values never masked (白名单), whatever their letter case; at most mask.MaxAllow.
	MaskAllow []string
	// SecurityWarnings shows potential spoofing and authentication failure warnings
	// in the reader; on by default.
	SecurityWarnings bool
	// VerifiedBadge shows a verified badge for authenticated senders (DKIM/DMARC);
	// on by default.
	VerifiedBadge bool
	// SecurityAI analyzes unverified email contents with the configured judge
	// model, which may be remote, to detect fraud and phishing; off by default.
	SecurityAI bool
	// RuleAI lets rules take AI actions (让 Wit 起草回复, 让 Wit 提取要点),
	// which send the mail to the default model, masked, unattended
	// (docs/adr/0030-mail-rules.md); off by default (ADR 0010).
	RuleAI bool
	// RecognitionModel is 识别模型 (docs/adr/0028-recognition-model-for-masking.md): a
	// model on this device finds names, addresses, accounts, phone numbers,
	// email addresses, secrets and card numbers the rules can't. Off by default; it turns on only once its
	// files are here (ErrRecognitionModelMissing), and turns names on with it.
	RecognitionModel bool
	// LabelDisplay is how label chips show: LabelDisplayBoth (the default;
	// "" too), LabelDisplayIcon or LabelDisplayText.
	LabelDisplay string
	// SidebarWidth is the page sidebars' width in DIPs, the user's drag:
	// 0 for the default, else 240 to 480.
	SidebarWidth int
	// ListWidth is the mail list's width in DIPs, the user's drag: 0 for the
	// default, else 280 to 560.
	ListWidth int
	// RemoteImages is when a message's remote images and sender avatars load, through the image
	// proxy: "auto" (default, "" too) at once, "trusted" for the trusted
	// senders only, "ask" after 加载图片. Mail in Junk never loads them by itself.
	RemoteImages string
	// ProxyMode is what mail, remote images, unsubscribes and the update
	// check connect through (网络代理, Service.Proxy): ProxySystem (the
	// default; "" too), ProxyCustom or ProxyOff. Only the user sets it: a
	// proxy reroutes mail.
	ProxyMode string
	// ProxyURL is 自定义's proxy, http://host:port or socks5://host:port
	// (ParseProxyURL), with no credentials. Checked only in ProxyCustom: the
	// other modes keep it as it is, so switching back loses nothing.
	ProxyURL string
	// ProxyBypass is 自定义's 不使用代理的主机: names, *-patterns, .suffixes,
	// addresses and CIDR ranges (sysproxy.Bypassed), at most MaxProxyBypass.
	ProxyBypass []string
	// ProxyUser is 自定义's username, "" for none.
	ProxyUser string
	// ProxyPassword is write-only: a SetSettings with one puts it in the
	// keychain (proxyKeyringUser); it is never stored here nor handed back.
	// ProxyPasswordSet says the keychain has one; a save turning it off
	// deletes it, and an empty ProxyPassword keeps it as it is. So a client
	// must send back the ProxyPasswordSet it loaded: a zero one clears the
	// password.
	ProxyPassword    string `json:",omitempty"`
	ProxyPasswordSet bool
	// SettingsVersion is 2 once the default text grew from 13 to 14 px and a
	// stored FontSize was raised by one to keep its sizes; 3 once a stored
	// BodyFontSize of 14, the old default, became 0 (follow FontSize); 4 once
	// Chinese language tags name the script (zh-CN → zh-Hans, zh-TW → zh-Hant).
	SettingsVersion int
}

const settingsVersion = 4

// chineseScriptTag is the script tag a region-tagged Chinese setting became
// in settings version 4.
var chineseScriptTag = map[string]string{"zh-CN": "zh-Hans", "zh-TW": "zh-Hant"}

func scriptTag(tag string) string {
	if t, ok := chineseScriptTag[tag]; ok {
		return t
	}
	return tag
}

// MinJudgeThreshold and MaxJudgeThreshold bound the user-adjustable cutoff.
const (
	MinJudgeThreshold = 0.5
	MaxJudgeThreshold = 0.95
)

// Label chips' looks (Settings.LabelDisplay): icon and name, icon only, name only.
const (
	LabelDisplayBoth = "both"
	LabelDisplayIcon = "icon"
	LabelDisplayText = "text"
)

// MinThreadCollapse and MaxThreadCollapse bound ThreadCollapseThreshold.
const (
	MinThreadCollapse     = 2
	MaxThreadCollapse     = 100
	DefaultThreadCollapse = 5
)

func defaultSettings() Settings {
	return Settings{Theme: "system", Density: "compact", FontSize: 14, SettingsVersion: settingsVersion, ReplyLanguage: "match", RemoteImages: "auto",
		KeepInBackground: true, ShowTrayIcon: true, UnreadOnAppIcon: true, NotifyNewMail: true, NotifyPreview: true, ThreadView: true, LightBackground: true,
		ThreadCollapse: true, ThreadCollapseThreshold: DefaultThreadCollapse, AutoLabel: true, LabelDisplay: LabelDisplayBoth, JudgeThreshold: 0.8,
		MCPSend: MCPSendConfirm, Masking: true, MaskCategories: defaultMaskCategories(),
		SecurityWarnings: true, VerifiedBadge: true, SecurityAI: false, ProxyMode: ProxySystem}
}

var langRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// validate says what is wrong with st; a masking rule in kept, stored
// before, may keep a built-in kind (mask.Check).
func (st Settings) validate(kept []mask.Rule) error {
	in := func(v string, ok ...string) bool {
		for _, o := range ok {
			if v == o {
				return true
			}
		}
		return false
	}
	switch {
	case !in(st.Theme, "system", "light", "dark"):
		return fmt.Errorf("invalid theme %q", st.Theme)
	case !in(st.Density, "compact", "comfortable"):
		return fmt.Errorf("invalid density %q", st.Density)
	case !in(st.RemoteImages, "", "auto", "trusted", "ask"):
		return fmt.Errorf("invalid remote images mode %q", st.RemoteImages)
	case !in(st.MCPSend, "", MCPSendOff, MCPSendConfirm, MCPSendDirect):
		return fmt.Errorf("invalid MCP send level %q", st.MCPSend)
	case !in(st.ProxyMode, "", ProxySystem, ProxyCustom, ProxyOff):
		return fmt.Errorf("invalid proxy mode %q", st.ProxyMode)
	case len(st.ProxyBypass) > MaxProxyBypass:
		return fmt.Errorf("at most %d hosts can skip the proxy", MaxProxyBypass)
	case len(st.ProxyUser) > 255 || len(st.ProxyPassword) > 255: // SOCKS5's limit (RFC 1929)
		return errors.New("proxy username and password must be at most 255 bytes")
	}
	if st.ProxyMode == ProxyCustom {
		if _, err := ParseProxyURL(st.ProxyURL); err != nil {
			return fmt.Errorf("invalid custom proxy: %w", err)
		}
	}
	switch {
	case !in(st.ReplyLanguage, "match", "reading"):
		return fmt.Errorf("invalid reply language mode %q", st.ReplyLanguage)
	case !in(st.LabelDisplay, "", LabelDisplayBoth, LabelDisplayIcon, LabelDisplayText):
		return fmt.Errorf("invalid label display %q", st.LabelDisplay)
	case st.FontSize < 10 || st.FontSize > 32, st.BodyFontSize != 0 && (st.BodyFontSize < 10 || st.BodyFontSize > 32):
		return fmt.Errorf("font size must be between 10 and 32")
	case st.SidebarWidth != 0 && (st.SidebarWidth < 240 || st.SidebarWidth > 480):
		return fmt.Errorf("sidebar width must be between 240 and 480")
	case st.ListWidth != 0 && (st.ListWidth < 280 || st.ListWidth > 560):
		return fmt.Errorf("list width must be between 280 and 560")
	case !(st.JudgeThreshold >= MinJudgeThreshold && st.JudgeThreshold <= MaxJudgeThreshold):
		return fmt.Errorf("judge threshold must be between %g and %g", MinJudgeThreshold, MaxJudgeThreshold)
	case st.ThreadCollapseThreshold < MinThreadCollapse || st.ThreadCollapseThreshold > MaxThreadCollapse:
		return fmt.Errorf("thread collapse threshold must be between %d and %d", MinThreadCollapse, MaxThreadCollapse)
	}
	if name, ok := strings.CutPrefix(st.Translator, translatorPrefix); st.Translator != "" && (!ok || !skills.ValidName(name)) {
		return fmt.Errorf("invalid translator %q", st.Translator)
	}
	for name, c := range st.MaskCategories.All() {
		if !in(c.Action, "", MaskMask, MaskWarn, MaskBlock) {
			return fmt.Errorf("invalid masking action %q for %s", c.Action, name)
		}
	}
	if err := mask.Check(st.maskConfig(), kept...); err != nil {
		return err
	}
	if strings.Contains(st.JudgeService, "/") && !isPluginRef(st.JudgeService) {
		return fmt.Errorf("invalid judge service %q", st.JudgeService)
	}
	for _, r := range []string{st.DefaultModel, st.SmallModel} {
		if _, err := ai.ParseModelRef(r); r != "" && err != nil {
			return err
		}
		if strings.HasPrefix(r, translatorPrefix) && !isPluginRef(r) {
			return fmt.Errorf("invalid plugin model reference %q", r)
		}
	}
	for _, l := range append([]string{st.UILanguage, st.ReadingLanguage}, st.NoTranslate...) {
		if l != "" && !langRE.MatchString(l) {
			return fmt.Errorf("invalid language tag %q", l)
		}
	}
	return nil
}

// GetSettings returns the stored settings, defaults for anything unset. Fields
// a newer build added are ignored.
func (s *Service) GetSettings(ctx context.Context) (Settings, error) {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return Settings{}, err
	}
	defer end()
	return s.loadSettings(ctx)
}

func (s *Service) loadSettings(ctx context.Context) (Settings, error) {
	st := defaultSettings()
	rows, err := s.db.Query(ctx, "SELECT value FROM settings WHERE key = 'app'")
	if err != nil || len(rows.Values) == 0 {
		return st, err
	}
	raw := []byte(rows.Values[0][0].(string))
	var stored struct{ FontSize, SettingsVersion int } // the defaults must not stand in for the stored version
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Settings{}, fmt.Errorf("stored settings: %w", err)
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return Settings{}, fmt.Errorf("stored settings: %w", err)
	}
	if stored.SettingsVersion < 2 && stored.FontSize > 0 { // an absent size keeps the new default
		st.FontSize = min(stored.FontSize+1, 32)
	}
	if stored.SettingsVersion < 3 && st.BodyFontSize == 14 {
		st.BodyFontSize = 0
	}
	if stored.SettingsVersion < 4 {
		st.UILanguage, st.ReadingLanguage = scriptTag(st.UILanguage), scriptTag(st.ReadingLanguage)
		for i, l := range st.NoTranslate {
			st.NoTranslate[i] = scriptTag(l)
		}
	}
	st.JudgeThreshold = max(MinJudgeThreshold, min(MaxJudgeThreshold, st.JudgeThreshold)) // older builds allowed 0..1
	st.ThreadCollapseThreshold = max(MinThreadCollapse, min(MaxThreadCollapse, st.ThreadCollapseThreshold))
	st.SettingsVersion = settingsVersion
	return st, nil
}

// SetSettings validates and stores st, then announces settings.changed.
func (s *Service) SetSettings(ctx context.Context, st Settings) (Settings, error) {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return Settings{}, err
	}
	defer end()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	stored, err := s.loadSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	if err := st.validate(stored.MaskRules); err != nil { // an old rule under a built-in kind must not stop saving the rest
		return Settings{}, err
	}
	if st.RecognitionModel && !stored.RecognitionModel {
		if !s.RecognitionModelInstalled() {
			return Settings{}, ErrRecognitionModelMissing
		}
		st.MaskCategories.Names.On = true // the names it finds are why it is on; off, names stay as they are
	}
	password := st.ProxyPassword
	st.ProxyPassword, st.ProxyPasswordSet = "", password != "" || st.ProxyPasswordSet && stored.ProxyPasswordSet
	st.SettingsVersion = settingsVersion // a client's 0 must not make the next load raise FontSize again
	write := func(st Settings) error {
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		_, err = s.db.Execute(ctx, `INSERT INTO settings(key, value) VALUES ('app', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(b))
		return err
	}
	if err := write(st); err != nil {
		return Settings{}, err
	}
	// the keychain after the row, which goes back if it fails, so the two never disagree
	if err := s.saveProxyPassword(password, st, stored); err != nil {
		if rerr := write(stored); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return Settings{}, err
	}
	if password == "" && st.ProxyPasswordSet {
		password = s.proxy.Load().password // kept as it is
	}
	s.setProxy(st, password)
	s.maskingChanged(st)
	if securityOn(st) && (!securityOn(stored) || st.SecurityAI && !stored.SecurityAI) {
		s.wakeSecurityScanner()
	}
	s.bus.Publish(events.Event{Type: events.SettingsChanged})
	return st, nil
}

// SystemLanguage is the operating system language tag passed in Options.
func (s *Service) SystemLanguage() string { return s.opts.SystemLanguage }

// ReadingLanguage is the language Wit reads and writes in: the setting,
// else the UI language, else the system language, else "en".
func (s *Service) ReadingLanguage(ctx context.Context) string { return s.readingLanguage(ctx) }

// readingLanguage is ReadingLanguage, else UILanguage, else the system language, else "en".
func (s *Service) readingLanguage(ctx context.Context) string {
	st, _ := s.loadSettings(ctx)
	for _, l := range []string{st.ReadingLanguage, st.UILanguage, s.opts.SystemLanguage, "en"} {
		if l != "" {
			return l
		}
	}
	return "en"
}

const maxTrustedSenders = 1000

// TrustImageSender trusts the address of from (a From header) to load
// remote images; from must parse as an address.
func (s *Service) TrustImageSender(ctx context.Context, from string) error {
	a, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("invalid sender address: %w", err)
	}
	addr := strings.ToLower(a.Address)
	return s.editSenders(ctx, trustedSenders, func(l []string) ([]string, error) {
		if slices.Contains(l, addr) {
			return l, nil
		}
		if len(l) >= maxTrustedSenders {
			return nil, fmt.Errorf("at most %d trusted senders", maxTrustedSenders)
		}
		return append(l, addr), nil
	})
}

// UntrustImageSender stops trusting addr.
func (s *Service) UntrustImageSender(ctx context.Context, addr string) error {
	addr = strings.ToLower(strings.TrimSpace(addr))
	return s.editSenders(ctx, trustedSenders, func(l []string) ([]string, error) {
		return slices.DeleteFunc(l, func(a string) bool { return a == addr }), nil
	})
}

// The lists kept beside Settings, a settings row each holding a JSON array:
// only their own methods change them, so a page's stale Settings cannot
// undo a trust, a block or a plugin switch.
const (
	trustedSenders = "senders.trusted"
	blockedSenders = "senders.blocked"
	builtinsOff    = "builtins.disabled"
	skillsOff      = "skills.disabled"
)

// readList is the list stored under key; nil if none is.
func (s *Service) readList(ctx context.Context, key string) ([]string, error) {
	rows, err := s.db.Query(ctx, "SELECT value FROM settings WHERE key = ?", key)
	if err != nil || len(rows.Values) == 0 {
		return nil, err
	}
	var l []string
	return l, json.Unmarshal([]byte(store.Str(rows.Values[0][0])), &l)
}

func (s *Service) writeList(ctx context.Context, key string, l []string) error {
	stmt, err := listWrite(key, l)
	if err != nil {
		return err
	}
	return s.exec(ctx, stmt)
}

func listWrite(key string, list []string) (sqlite.Statement, error) {
	data, err := json.Marshal(list)
	return sqlite.Statement{SQL: `INSERT INTO settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, Args: []any{key, string(data)}}, err
}

// TrustedImageSenders are the lower-cased From addresses whose remote
// images load at once (始终信任此发件人).
func (s *Service) TrustedImageSenders(ctx context.Context) ([]string, error) {
	return s.list(ctx, trustedSenders)
}

// BlockedSenders are the addresses the user blocked, keyed as gomail.AddrKey.
func (s *Service) BlockedSenders(ctx context.Context) ([]string, error) {
	return s.list(ctx, blockedSenders)
}

func (s *Service) list(ctx context.Context, key string) ([]string, error) {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer end()
	return s.readList(ctx, key)
}

// editSenders changes the sender list under key, under settingsMu, then
// announces settings.changed.
func (s *Service) editSenders(ctx context.Context, key string, edit func([]string) ([]string, error)) error {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return err
	}
	defer end()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	l, err := s.readList(ctx, key)
	if err != nil {
		return err
	}
	if l, err = edit(l); err != nil {
		return err
	}
	stmt, err := listWrite(key, l)
	if err != nil {
		return err
	}
	stmts := []sqlite.Statement{stmt}
	if key == blockedSenders {
		stmts = append(stmts, sqlite.Statement{SQL: `UPDATE rule_action_results SET prior = json_set(prior, '$.had', json('true'))
			WHERE kind = ? AND state = ? AND json_extract(prior, '$.from_addr') NOT IN (SELECT value FROM json_each(?))`,
			Args: []any{ActionBlock, RuleActionDone, stmt.Args[1]}})
	}
	if _, err := s.db.Transaction(ctx, stmts); err != nil {
		return err
	}
	s.bus.Publish(events.Event{Type: events.SettingsChanged})
	return nil
}

const maxBlockedSenders = 1000

// BlockSender blocks the address of from (a From header), keyed as
// gomail.AddrKey so arrivals and the reader match it the same way.
func (s *Service) BlockSender(ctx context.Context, from string) error {
	addr := gomail.AddrKey(from)
	return s.editSenders(ctx, blockedSenders, func(list []string) ([]string, error) {
		return addBlockedSender(list, addr)
	})
}

func addBlockedSender(list []string, addr string) ([]string, error) {
	if addr == "" || !strings.Contains(addr, "@") {
		return nil, fmt.Errorf("invalid sender address: %q", addr)
	}
	if slices.Contains(list, addr) {
		return list, nil
	}
	if len(list) >= maxBlockedSenders {
		return nil, fmt.Errorf("at most %d blocked senders", maxBlockedSenders)
	}
	return append(list, addr), nil
}

// UnblockSender removes addr from the blocked senders list.
func (s *Service) UnblockSender(ctx context.Context, addr string) error {
	addr = gomail.AddrKey(addr)
	return s.editSenders(ctx, blockedSenders, func(l []string) ([]string, error) {
		return slices.DeleteFunc(l, func(a string) bool { return a == addr }), nil
	})
}

// inJunk is whether folder f is junk, detected or given by the user, even
// where another folder holds the role: ImagesAllowed and Message.InJunk agree.
const inJunk = "'junk' IN (f.role, f.role_override)"

// ImagesAllowed is whether message's remote images load without asking, by
// the RemoteImages setting: never in a junk folder (From is unauthenticated);
// "auto" otherwise; "trusted" for the trusted From addresses; "ask" never.
func (s *Service) ImagesAllowed(ctx context.Context, message int64) (bool, error) {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return false, err
	}
	defer end()
	message, err = s.resolveID(ctx, message)
	if err != nil {
		return false, err
	}
	rows, err := s.db.Query(ctx, "SELECT m.from_addr, "+inJunk+" FROM messages m JOIN folders f ON f.id = "+shownFolder+" WHERE m.id = ?", message)
	if err != nil || len(rows.Values) == 0 || store.Int(rows.Values[0][1]) != 0 {
		return false, err
	}
	st, err := s.loadSettings(ctx)
	if err != nil {
		return false, err
	}
	switch st.RemoteImages {
	case "", "auto":
		return true, nil
	case "trusted":
		a, err := mail.ParseAddress(store.Str(rows.Values[0][0]))
		if err != nil {
			return false, nil
		}
		trusted, err := s.readList(ctx, trustedSenders)
		return slices.Contains(trusted, strings.ToLower(a.Address)), err
	}
	return false, nil
}
