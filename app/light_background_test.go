package app

import (
	"context"
	"testing"

	"github.com/godwit-email/godwit/internal/i18n"
)

// The window in front is what ends the narrow sync: the setting alone does not.
func TestSetForegroundNarrowsSync(t *testing.T) {
	h := newHarness(t)
	h.until("settings loaded", func() bool { return h.app.loaded })
	h.app.settings.LightBackground = true

	h.app.SetForeground(false)
	if !h.env.Svc.LightBackground() {
		t.Fatal("the background did not narrow the sync")
	}
	h.app.SetForeground(true)
	if h.env.Svc.LightBackground() {
		t.Fatal("the window in front kept the sync narrow")
	}

	h.app.settings.LightBackground = false
	h.app.SetForeground(false)
	if h.env.Svc.LightBackground() {
		t.Fatal("the setting off narrowed the sync")
	}
}

// The setting has its row under Startup, and it is saved as any other switch.
func TestSettingsLightBackgroundToggle(t *testing.T) {
	h, _ := withAccount(t)
	h.openSettings("general")
	h.see(i18n.T("settings.light_background"))

	h.click(i18n.T("settings.light_background"))
	h.until("the setting saved off", func() bool {
		st, err := h.env.Svc.GetSettings(context.Background())
		return err == nil && !st.LightBackground
	})
}

// Turning the setting off while the window is behind another app widens the
// sync at once, without waiting for a focus change.
func TestLightBackgroundSettingApplies(t *testing.T) {
	h := newHarness(t)
	h.until("settings loaded", func() bool { return h.app.loaded })
	h.app.foreground = false
	off := h.app.settings
	off.LightBackground = false
	on := off
	on.LightBackground = true

	h.app.settingsEffects(off, on)
	if !h.env.Svc.LightBackground() {
		t.Fatal("the setting on did not narrow the sync")
	}
	h.app.settingsEffects(on, off)
	if h.env.Svc.LightBackground() {
		t.Fatal("the setting off kept the sync narrow")
	}
}
