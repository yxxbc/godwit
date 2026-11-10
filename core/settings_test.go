package core

import (
	"math"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

func settingsService(t *testing.T, dir, sys string) *Service {
	t.Helper()
	keyring.MockInit()
	s, err := New(t.Context(), Options{DataDir: dir, SystemLanguage: sys})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSettingsDefaults(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	got, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Settings{Theme: "system", Density: "compact", FontSize: 14, SettingsVersion: 4, ReplyLanguage: "match",
		KeepInBackground: true, ShowTrayIcon: true, UnreadOnAppIcon: true, NotifyNewMail: true, NotifyPreview: true, ThreadView: true, ThreadCollapse: true, ThreadCollapseThreshold: 5, RemoteImages: "auto", AutoLabel: true, LabelDisplay: "both", JudgeThreshold: 0.8, LightBackground: true,
		MCPSend: "confirm", Masking: true, MaskCategories: defaultMaskCategories(),
		SecurityWarnings: true, VerifiedBadge: true, ProxyMode: "system"}, got)
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := settingsService(t, dir, "")
	ch, stop := s.Subscribe()
	defer stop()
	in, _ := s.GetSettings(t.Context())
	in.Theme, in.UILanguage, in.NoTranslate, in.DefaultModel = "dark", "zh-Hans", []string{"en", "fr"}, "svc/m"
	in.UnreadOnAppIcon = false
	in.JudgeThreshold = 0.83
	in.ProxyMode = ProxyOff
	_, err := s.SetSettings(t.Context(), in)
	require.NoError(t, err)
	select {
	case ev := <-ch:
		assert.Equal(t, events.SettingsChanged, ev.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("no settings.changed event")
	}
	require.NoError(t, s.Close())

	s = settingsService(t, dir, "")
	got, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, in, got)

	// A newer build's extra field does not break reading.
	_, err = s.db.Execute(t.Context(), "UPDATE settings SET value = ? WHERE key = 'app'", `{"Theme":"light","Future":1}`)
	require.NoError(t, err)
	got, err = s.GetSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "light", got.Theme)
	assert.Equal(t, 14, got.FontSize)
	assert.True(t, got.UnreadOnAppIcon, "older settings default to showing the app badge")
	assert.True(t, got.ShowTrayIcon, "older settings default to showing the menu bar icon")
	assert.Equal(t, 0.8, got.JudgeThreshold, "settings with no threshold use the default")
	assert.Equal(t, ProxySystem, got.ProxyMode, "settings stored before the proxy setting follow the system")
}

func TestSettingsJudgeThresholdBounds(t *testing.T) {
	for raw, want := range map[string]float64{
		`{}`:                      0.8,
		`{"JudgeThreshold":0}`:    0.5,
		`{"JudgeThreshold":0.3}`:  0.5,
		`{"JudgeThreshold":0.49}`: 0.5,
		`{"JudgeThreshold":0.5}`:  0.5,
		`{"JudgeThreshold":0.7}`:  0.7,
		`{"JudgeThreshold":0.9}`:  0.9,
		`{"JudgeThreshold":0.95}`: 0.95,
		`{"JudgeThreshold":1}`:    0.95,
	} {
		t.Run(raw, func(t *testing.T) {
			s := settingsService(t, t.TempDir(), "")
			_, err := s.db.Execute(t.Context(), "INSERT INTO settings(key, value) VALUES ('app', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", raw)
			require.NoError(t, err)
			st, err := s.GetSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, want, st.JudgeThreshold)
			_, err = s.SetSettings(t.Context(), st)
			require.NoError(t, err)
		})
	}
}

// Version 4 names the script of a stored region-tagged Chinese setting;
// a version-4 setting is left as the user chose it.
func TestSettingsChineseScriptTags(t *testing.T) {
	for raw, want := range map[string][3]any{
		`{"SettingsVersion":3,"UILanguage":"zh-CN","ReadingLanguage":"zh-TW","NoTranslate":["en","zh-TW","zh-CN"]}`: {"zh-Hans", "zh-Hant", []string{"en", "zh-Hant", "zh-Hans"}},
		`{"UILanguage":"zh-TW"}`:                          {"zh-Hant", "", []string(nil)},
		`{"SettingsVersion":4,"ReadingLanguage":"zh-CN"}`: {"", "zh-CN", []string(nil)},
	} {
		t.Run(raw, func(t *testing.T) {
			s := settingsService(t, t.TempDir(), "")
			_, err := s.db.Execute(t.Context(), "INSERT INTO settings(key, value) VALUES ('app', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", raw)
			require.NoError(t, err)
			st, err := s.GetSettings(t.Context())
			require.NoError(t, err)
			assert.Equal(t, want, [3]any{st.UILanguage, st.ReadingLanguage, st.NoTranslate})
		})
	}
}

func TestSettingsValidation(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	base, _ := s.GetSettings(t.Context())
	base.Theme, base.FontSize, base.UILanguage, base.SidebarWidth = "dark", 20, "fr", 480
	_, err := s.SetSettings(t.Context(), base)
	require.NoError(t, err)
	for name, mod := range map[string]func(*Settings){
		"theme":          func(x *Settings) { x.Theme = "neon" },
		"density":        func(x *Settings) { x.Density = "tiny" },
		"reply":          func(x *Settings) { x.ReplyLanguage = "other" },
		"labeldisplay":   func(x *Settings) { x.LabelDisplay = "invalid" },
		"proxy":          func(x *Settings) { x.ProxyMode = "socks" },
		"font":           func(x *Settings) { x.FontSize = 9 },
		"bodyfont":       func(x *Settings) { x.BodyFontSize = 33 },
		"lang":           func(x *Settings) { x.UILanguage = "zh_CN!" },
		"notrans":        func(x *Settings) { x.NoTranslate = []string{"en", "x y"} },
		"narrow":         func(x *Settings) { x.SidebarWidth = 239 },
		"wide":           func(x *Settings) { x.SidebarWidth = 481 },
		"list":           func(x *Settings) { x.ListWidth = 279 },
		"listwide":       func(x *Settings) { x.ListWidth = 561 },
		"threshold low":  func(x *Settings) { x.JudgeThreshold = 0.4999 },
		"threshold high": func(x *Settings) { x.JudgeThreshold = 0.9501 },
		"threshold NaN":  func(x *Settings) { x.JudgeThreshold = math.NaN() },
		"threshold inf":  func(x *Settings) { x.JudgeThreshold = math.Inf(1) },
	} {
		in := base
		mod(&in)
		_, err := s.SetSettings(t.Context(), in)
		assert.Error(t, err, name)
	}
	got, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, base, got)
}

// 自定义 needs a usable address; the other modes keep whatever was typed.
func TestSettingsProxyURL(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	st, _ := s.GetSettings(t.Context())
	st.ProxyMode = ProxyCustom
	for _, ok := range []string{"socks5://127.0.0.1:7890", "http://proxy.example:8080", "socks5://[::1]:1080"} {
		st.ProxyURL = ok
		_, err := s.SetSettings(t.Context(), st)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "socks5://:1080", "socks5://h", "socks5://h:0", "socks5://h:65536", "socks5://h:1080/x",
		"socks5://u:p@h:1080", "https://h:443", "socks4://h:1080", "h:1080", "socks5://h:1080?a", "socks5://[a:b]:1080"} {
		in := st
		in.ProxyURL = bad
		_, err := s.SetSettings(t.Context(), in)
		assert.Error(t, err, bad)
		for _, mode := range []string{ProxySystem, ProxyOff} {
			in.ProxyMode = mode
			got, err := s.SetSettings(t.Context(), in)
			if assert.NoError(t, err, "%s %q", mode, bad) {
				assert.Equal(t, bad, got.ProxyURL, "kept in %s", mode)
			}
		}
	}
}

// 不使用代理的主机 round-trips, up to MaxProxyBypass entries.
func TestSettingsProxyBypass(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	st, _ := s.GetSettings(t.Context())
	st.ProxyBypass = []string{"*.corp.example", ".lan", "mail.home", "10.0.0.0/8", "192.168.1.5"}
	_, err := s.SetSettings(t.Context(), st)
	require.NoError(t, err)
	got, err := s.GetSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, st.ProxyBypass, got.ProxyBypass)

	st.ProxyBypass = make([]string, 201)
	for i := range st.ProxyBypass {
		st.ProxyBypass[i] = "h.example"
	}
	_, err = s.SetSettings(t.Context(), st)
	assert.Error(t, err, "an over-long list")
}

func TestReadingLanguageFallback(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	assert.Equal(t, "en", s.readingLanguage(t.Context()))
	s = settingsService(t, t.TempDir(), "de")
	assert.Equal(t, "de", s.readingLanguage(t.Context()))
	in, _ := s.GetSettings(t.Context())
	in.UILanguage = "fr"
	_, err := s.SetSettings(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, "fr", s.readingLanguage(t.Context()))
	in.ReadingLanguage = "ja"
	_, err = s.SetSettings(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, "ja", s.readingLanguage(t.Context()))
}

func TestSettingsValidateModelRefs(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	st, _ := s.GetSettings(t.Context())
	st.DefaultModel, st.SmallModel = "nowhere/m1", "nowhere/m2" // format only, no lookup
	_, err := s.SetSettings(t.Context(), st)
	require.NoError(t, err)
	for _, bad := range []string{"nomodel", "/m", "svc/"} {
		in := st
		in.DefaultModel = bad
		_, err = s.SetSettings(t.Context(), in)
		assert.Error(t, err, "default %q", bad)
		in = st
		in.SmallModel = bad
		_, err = s.SetSettings(t.Context(), in)
		assert.Error(t, err, "small %q", bad)
	}
}

func TestSettingsFontMigration(t *testing.T) {
	for stored, want := range map[string][2]int{
		`{"FontSize":13,"BodyFontSize":15}`: {14, 15},
		`{"Theme":"light"}`:                 {14, 0},
		`{"FontSize":32}`:                   {32, 0},
		`{"FontSize":16}`:                   {17, 0},
		`{"FontSize":13,"BodyFontSize":15,"SettingsVersion":2}`: {13, 15},
		`{"FontSize":14,"BodyFontSize":14,"SettingsVersion":2}`: {14, 0},
		`{"FontSize":13,"BodyFontSize":14}`:                     {14, 0},
		`{"FontSize":16,"BodyFontSize":14,"SettingsVersion":3}`: {16, 14},
	} {
		s := settingsService(t, t.TempDir(), "")
		_, err := s.db.Execute(t.Context(), "INSERT INTO settings(key, value) VALUES ('app', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", stored)
		require.NoError(t, err)
		got, err := s.GetSettings(t.Context())
		require.NoError(t, err)
		assert.Equal(t, want, [2]int{got.FontSize, got.BodyFontSize}, stored)
		assert.Equal(t, settingsVersion, got.SettingsVersion, stored)
	}
}

func TestSettingsVersionStamped(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	in, _ := s.GetSettings(t.Context())
	in.FontSize, in.SettingsVersion = 15, 0
	_, err := s.SetSettings(t.Context(), in)
	require.NoError(t, err)
	for range 2 { // load -> save -> load is stable
		got, err := s.GetSettings(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 15, got.FontSize)
		_, err = s.SetSettings(t.Context(), got)
		require.NoError(t, err)
	}
}

func TestSettingsTypeMismatchErrors(t *testing.T) {
	s := settingsService(t, t.TempDir(), "")
	_, err := s.db.Execute(t.Context(), "INSERT INTO settings(key, value) VALUES ('app', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", `{"FontSize":14,"Theme":5}`)
	require.NoError(t, err)
	_, err = s.GetSettings(t.Context())
	assert.ErrorContains(t, err, "stored settings")
}
