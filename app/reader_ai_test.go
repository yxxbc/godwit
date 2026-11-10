package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/ai"
	"github.com/godwit-email/godwit/core/coretest"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/richtext"
)

// aiHarness is a mail harness with one account; with llm a fake model
// service is the default model. set changes the settings before the App starts.
func aiHarness(t *testing.T, llm bool, set func(*core.Settings), setup ...func(*coretest.Env)) (*harness, core.Account, *coretest.LLM) {
	t.Helper()
	var acc core.Account
	var l *coretest.LLM
	h := newHarnessWith(t, nil, append([]func(*coretest.Env){func(e *coretest.Env) {
		acc = e.AddAccount(t, "alice@example.com")
		if llm {
			l = e.LLM(t)
			t.Cleanup(l.Release)
		}
		st, err := e.Svc.GetSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		st.Summary, st.QuickReplies = true, true // off by default; these tests read them unless set turns them off
		if set != nil {
			set(&st)
		}
		if _, err := e.Svc.SetSettings(context.Background(), st); err != nil {
			t.Fatal(err)
		}
	}}, setup...)...)
	h.until("the mail page", func() bool { return h.app.loaded && h.app.route.Page == PageMail })
	return h, acc, l
}

func noQuick(st *core.Settings)   { st.QuickReplies = false }
func noSummary(st *core.Settings) { st.Summary = false }
func noAuto(st *core.Settings)    { st.QuickReplies, st.Summary = false, false }

// gateAfterFirstText makes r's skill runs pause after their first text
// event until the returned release is called.
func gateAfterFirstText(r *reader) (release func()) {
	gate := make(chan struct{})
	real := r.runSkill
	r.runSkill = func(ctx context.Context, name string, tg core.Target) (<-chan ai.Event, error) {
		in, err := real(ctx, name, tg)
		if err != nil {
			return nil, err
		}
		out := make(chan ai.Event)
		go func() {
			defer close(out)
			first := true
			for ev := range in {
				out <- ev
				if ev.Kind == "text" && first {
					first = false
					<-gate
				}
			}
		}()
		return out, nil
	}
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func TestSummaryStreams(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noQuick)
	llm.Script(coretest.Turn{Text: []string{"- **第一点要点**\n", "- [第二点要点](https://summary.example/plan)\n"}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	other := h.deliverOne(acc, coretest.Msg("carol@example.com", acc.Email, "Other", "other body"))
	r := h.app.mail.reader
	release := gateAfterFirstText(r)
	t.Cleanup(release)
	h.openThread(m.ThreadID)
	h.see("m1")
	h.see("第一点要点")
	if h.tt.HasText("**") {
		t.Fatalf("streaming summary shows Markdown markers: %q", h.tt.Texts())
	}
	if h.tt.HasText("第二点要点") || !r.ai[m.ID].summary.running {
		t.Fatalf("the second point before it streamed: %q", h.tt.Texts())
	}
	release()
	h.see("第二点要点")
	h.until("the summary done", func() bool { s := r.ai[m.ID]; return s != nil && s.summary.done })
	clickAIOutputLink(h, "第二点要点")
	if !h.app.linkOpen || h.app.link != "https://summary.example/plan" {
		t.Fatalf("summary link did not use the link dialog: %v %q", h.app.linkOpen, h.app.link)
	}
	h.click("取消")
	n := llm.Requests()
	h.openThread(other.ThreadID)
	h.seeBody(other.ID)
	h.until("the other summary done", func() bool { s := r.ai[other.ID]; return s != nil && s.summary != nil && s.summary.done })
	n2 := llm.Requests()
	if n2 != n+1 {
		t.Fatalf("requests %d after the other message, want %d", n2, n+1)
	}
	h.openThread(m.ThreadID)
	h.see("第二点要点")
	if got := llm.Requests(); got != n2 {
		t.Fatalf("requests %d after coming back, want %d", got, n2)
	}
}

func TestSkillMarkdownStreams(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto, func(e *coretest.Env) { writeSkill(t, e, "brief") })
	llm.Script(coretest.Turn{Text: []string{"## **Progress", "**\n\n[Read more](https://skill.example/plan)\n\n![Private image](https://image.example/pixel.png)"}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "body words"))
	r := h.app.mail.reader
	release := gateAfterFirstText(r)
	t.Cleanup(release)
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	h.skillMenu("brief")
	h.see("Progress")
	if h.tt.HasText("##") || h.tt.HasText("**") || !r.ai[m.ID].result.run.running {
		t.Fatalf("unfinished Markdown was not rendered while streaming: %q", h.tt.Texts())
	}
	release()
	h.until("the skill done", func() bool { return r.ai[m.ID].result.run.done })
	h.see("Private image")
	if h.tt.HasText("##") || h.tt.HasText("**") || h.tt.HasText("![") {
		t.Fatalf("completed output shows Markdown markers: %q", h.tt.Texts())
	}
	heading, _ := h.tt.Find("Progress")
	h.setSettings(func(st *core.Settings) { st.BodyFontSize = 20 })
	larger, _ := h.tt.Find("Progress")
	if larger.H <= heading.H {
		t.Fatalf("Markdown ignored the reading font size: before %v, after %v", heading.H, larger.H)
	}
	clickAIOutputLink(h, "Read more")
	if !h.app.linkOpen || h.app.link != "https://skill.example/plan" {
		t.Fatalf("skill link did not use the link dialog: %v %q", h.app.linkOpen, h.app.link)
	}
}

func TestAIOutputKeepsPluginHTML(t *testing.T) {
	h, acc, _ := aiHarness(t, false, noAuto)
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "body words"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	doc, _ := richtext.FromMailHTML(`<p><strong>Plugin result</strong></p><p><a href="https://plugin.example/plan">Plugin link</a></p>`, richtext.DefaultLimits)
	h.app.mail.reader.state(m.ID).result = &aiResult{title: "Plugin", run: &aiRun{text: "fallback text", doc: doc, done: true}}
	h.see("Plugin result")
	if h.tt.HasText("fallback text") || h.tt.HasText("<strong>") {
		t.Fatalf("plugin HTML was not rendered: %q", h.tt.Texts())
	}
	h.see("Plugin link")
}

// Rich-text paragraphs span the whole row, so click within the link's glyphs.
func clickAIOutputLink(h *harness, text string) {
	h.t.Helper()
	h.see(text)
	box, ok := h.tt.Find(text)
	if !ok {
		h.t.Fatalf("no link %q", text)
	}
	h.tt.ClickAt(box.X+5, box.Y+box.H/2)
	h.drain()
	h.tt.Frame()
}

// setSettings changes the settings in the Service and in the App, as a
// save from the settings page does.
func (h *harness) setSettings(f func(*core.Settings)) {
	h.t.Helper()
	st, err := h.env.Svc.GetSettings(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	f(&st)
	if _, err := h.env.Svc.SetSettings(context.Background(), st); err != nil {
		h.t.Fatal(err)
	}
	f(&h.app.settings)
	f(&h.app.settingsOK)
	h.app.refreshTasks() // as a settings save in the App does
	h.tt.Frame()
}

// thread2 puts a two-message unread thread in the INBOX and returns its messages.
func (h *harness) thread2(acc core.Account) []core.MessageSummary {
	h.t.Helper()
	base := h.clock.Now()
	h.put(acc, "INBOX", coretest.Msg("bob@example.com", acc.Email, "Plan", "first words", coretest.MessageID("a@x"), at(base, 2)))
	h.put(acc, "INBOX", coretest.Msg("carol@example.com", acc.Email, "Re: Plan", "second words", coretest.MessageID("b@x"), coretest.InReplyTo("a@x"), at(base, 1)))
	h.syncNow(acc)
	page, err := h.env.Svc.ListThreads(context.Background(), core.View{Unified: "inbox"}, "", 10)
	if err != nil || len(page.Items) != 1 {
		h.t.Fatalf("threads %+v, %v", page.Items, err)
	}
	return h.messages(page.Items[0].ID)
}

// A thread summarizes only its newest message, the one it shows under the
// subject: an older expanded one would spend tokens on nothing shown.
func TestSummaryNewestOnly(t *testing.T) {
	h, acc, _ := aiHarness(t, true, noQuick)
	msgs := h.thread2(acc)
	r := h.app.mail.reader
	h.openThreadWith(msgs[0].ThreadID, msgs[0].ID)
	h.until("the newest summary done", func() bool {
		s := r.ai[msgs[1].ID]
		return s != nil && s.summary != nil && s.summary.done
	})
	if !r.open[msgs[0].ID] {
		t.Fatal("the older message folded")
	}
	if s := r.ai[msgs[0].ID]; s != nil && s.summary != nil {
		t.Fatal("the older expanded message was summarized")
	}
	if n := h.countTexts("m1"); n != 1 {
		t.Fatalf("%d summaries shown: %q", n, h.tt.Texts())
	}
}

func TestTranslationStaysWithMessage(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto)
	llm.Script(coretest.Turn{Text: []string{`{"texts":["Die übersetzten Worte der ersten Nachricht hier."],"source":"en"}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "first words", coretest.MessageID("p1@x"), at(h.clock.Now(), 2)))
	h.openThread(m.ThreadID)
	v := h.seeBody(m.ID)
	h.click("翻译")
	h.seeTranslation(m.ID, "übersetzten")
	h.env.Deliver(t, acc.ID, "INBOX", coretest.Msg("carol@example.com", acc.Email, "Re: Plan", "reply words",
		coretest.MessageID("p2@x"), coretest.InReplyTo("p1@x"), at(h.clock.Now(), 1)))
	r := h.app.mail.reader
	h.waitMail("the reply shown", func() bool {
		if len(r.msgs) != 2 {
			return false
		}
		_, ok := h.viewBox(h.plat.view(r.msgs[1].ID))
		return ok
	})
	h.seeTranslation(m.ID, "übersetzten")
	h.see("显示原文")
	if _, ok := h.viewBox(v); !ok {
		t.Fatal("the older message lost its translation")
	}
	h.click("显示原文")
	if h.seeBody(m.ID) != v {
		t.Fatal("the original is a new page")
	}
	if len(v.translations) != 0 {
		t.Fatal("the translation still shows")
	}
}

func TestSummaryFollowsSettings(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noQuick)
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	r := h.app.mail.reader
	h.openThread(m.ThreadID)
	h.until("the summary done", func() bool { s := r.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.see("m1")
	n := llm.Requests()
	h.setSettings(func(st *core.Settings) { st.Summary = false })
	h.until("the summary hidden", func() bool { return !h.tt.HasText("summarize") })
	h.click("总结")
	h.see("m1")
	if got := llm.Requests(); got != n {
		t.Fatalf("requests %d, want %d: the summary ran again", got, n)
	}
	h.setSettings(func(st *core.Settings) { st.DefaultModel = "" })
	h.until("the summary hidden without a model", func() bool { return !h.tt.HasText("summarize") && !h.tt.HasText("总结") })
}

func TestSummaryOnDemand(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto)
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	if h.tt.HasText("summarize") || llm.Requests() != 0 {
		t.Fatal("a summary ran with Summary off")
	}
	h.click("总结")
	h.see("m1")
	h.until("one request", func() bool { return llm.Requests() == 1 })
	h.click("总结")
	h.until("hidden again", func() bool { return !h.tt.HasText("summarize") })
}

func TestTranslationFollowsLanguageAndModel(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto)
	llm.Script(coretest.Turn{Text: []string{`{"texts":["Hallo Welt, das ist die erste Übersetzung hier."],"source":"en"}`}},
		coretest.Turn{Text: []string{`{"texts":["こんにちは世界、これは二番目の翻訳です。ここにあります。"],"source":"en"}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "the original words"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	h.click("翻译")
	h.see("已翻译为中文 · m1")
	h.seeTranslation(m.ID, "erste Übersetzung")
	h.app.settings.DefaultModel = "test/m2" // a later model does not relabel the result
	h.tt.Frame()
	h.see("已翻译为中文 · m1")
	h.app.settings.DefaultModel = "test/m1"
	h.setSettings(func(st *core.Settings) { st.ReadingLanguage = "ja" })
	v := h.seeBody(m.ID) // the original, not the other language's translation
	if len(v.translations) != 0 || h.tt.HasText("已翻译为") {
		t.Fatalf("a translation into another language shown: %q", h.tt.Texts())
	}
	h.click("翻译")
	h.see("已翻译为日文 · m1")
	h.seeTranslation(m.ID, "二番目の翻訳")
	if n := llm.Requests(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

func TestCardsFailRetry(t *testing.T) {
	h, acc, _ := aiHarness(t, false, nil, func(e *coretest.Env) { e.InstallPlugin(t, "card-demo") })
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Invoice 42", "pay me"))
	r := h.app.mail.reader
	var failed atomic.Bool
	real := r.cardsFn
	r.cardsFn = func(ctx context.Context, id int64) ([]core.Card, error) {
		if failed.CompareAndSwap(false, true) {
			return nil, errors.New("cards went away")
		}
		return real(ctx, id)
	}
	h.openThread(m.ThreadID)
	h.see("cards went away")
	h.click("重试")
	h.see("Invoice · 来自插件 card-demo")
	if h.tt.HasText("cards went away") {
		t.Fatal("the error stays")
	}
}

func TestQuickRepliesFailRetry(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noSummary)
	llm.Script(coretest.Turn{Text: []string{`{"replies":[{"html":"<p>Sure thing</p>"}]}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Question", "can you?"))
	r := h.app.mail.reader
	var failed atomic.Bool
	real := r.quickFn
	r.quickFn = func(ctx context.Context, id int64) ([]core.QuickReply, error) {
		if failed.CompareAndSwap(false, true) {
			return nil, errors.New("replies went away")
		}
		return real(ctx, id)
	}
	h.openThread(m.ThreadID)
	h.see("replies went away")
	h.click("重试")
	h.see("Sure thing")
}

func TestMenuSourcesIndependent(t *testing.T) {
	h, acc, _ := aiHarness(t, true, noAuto, func(e *coretest.Env) {
		e.InstallPlugin(t, "egress-action")
		writeSkill(t, e, "brief")
	})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "body words"))
	other := h.deliverOne(acc, coretest.Msg("carol@example.com", acc.Email, "Other", "other body"))
	r := h.app.mail.reader
	var failed atomic.Bool
	real := r.skillsFn
	r.skillsFn = func(ctx context.Context) ([]core.SkillInfo, error) {
		if failed.CompareAndSwap(false, true) {
			return nil, errors.New("skills went away")
		}
		return real(ctx)
	}
	h.openThread(m.ThreadID)
	h.see("skills went away") // a toast
	h.click("更多 skill")
	menu := h.tt.Menu()
	h.tt.CloseMenu()
	if !slices.Contains(menu, "Ping") || slices.Contains(menu, "brief") {
		t.Fatalf("menu %q", menu)
	}
	h.openThread(other.ThreadID) // read again on the next visit
	h.seeBody(other.ID)
	h.until("the skills read", func() bool { return r.skills.done })
	h.click("更多 skill")
	menu = h.tt.Menu()
	h.tt.CloseMenu()
	if !slices.Contains(menu, "Ping") || !slices.Contains(menu, "brief") {
		t.Fatalf("menu %q", menu)
	}
}

func TestSummaryOffWithoutModel(t *testing.T) {
	h, acc, _ := aiHarness(t, false, nil)
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	for range 20 {
		h.drain()
		h.tt.Frame()
	}
	if h.tt.HasText("summarize") || h.tt.HasText("翻译") || h.tt.HasText("即时回复") {
		t.Fatalf("AI shown without a model: %q", h.tt.Texts())
	}
	if s := h.app.mail.reader.ai[m.ID]; s != nil && s.summary != nil {
		t.Fatal("a summary started without a model")
	}
}

func TestTranslateToggle(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto)
	llm.Script(coretest.Turn{Text: []string{`{"texts":["Hallo Welt, das ist die Übersetzung des Textes."],"source":"en"}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "the original words"))
	h.openThread(m.ThreadID)
	v := h.seeBody(m.ID)
	h.click("翻译")
	h.see("已翻译为中文 · m1")
	h.seeTranslation(m.ID, "Welt")
	if h.seeBody(m.ID) != v {
		t.Fatal("translation replaced the mail's body view")
	}
	h.click("显示原文")
	h.seeBody(m.ID)
	if len(v.translations) != 0 {
		t.Fatal("translation still shown")
	}
	h.click("翻译")
	h.seeTranslation(m.ID, "Welt")
	if n := llm.Requests(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// writeSkill adds a user skill on messages to the Env's skills directory.
func writeSkill(t *testing.T, e *coretest.Env, name string) {
	t.Helper()
	dir := filepath.Join(e.DataDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: Brief the email\nmetadata:\n  godwit-action-on: \"message\"\n  godwit-action-output: \"panel\"\n---\n\nBrief it.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillMenu chooses item from the reader's skill menu.
func (h *harness) skillMenu(item string) {
	h.t.Helper()
	h.click("更多 skill")
	if err := h.tt.ChooseMenuItem(item); err != nil {
		h.t.Fatalf("%v; menu %q", err, h.tt.Menu())
	}
	h.drain()
	h.tt.Frame()
}

func TestSkillMenuRuns(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto, func(e *coretest.Env) { writeSkill(t, e, "brief") })
	llm.Script(coretest.Turn{Text: []string{"the skill output"}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "body words"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	h.see("更多 skill")
	h.click("更多 skill")
	menu := strings.Join(h.tt.Menu(), "|")
	h.tt.CloseMenu()
	if strings.Contains(menu, "summarize") || strings.Contains(menu, "translate") || strings.Contains(menu, "proofread") {
		t.Fatalf("menu %q", menu)
	}
	h.skillMenu("brief")
	h.see("the skill output")
	h.click("关闭结果")
	h.until("the card closed", func() bool { return !h.tt.HasText("the skill output") })
}

func TestActionEgressApproval(t *testing.T) {
	h, acc, _ := aiHarness(t, false, nil, func(e *coretest.Env) { e.InstallPlugin(t, "egress-action") })
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "body words"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	h.skillMenu("Ping")
	h.see("egress.godwit.test:80")
	h.see("egress-action")
	if h.tt.HasText("pong") {
		t.Fatal("ran before approval")
	}
	h.click("允许这一次")
	h.see("pong")
}

func TestCardCopy(t *testing.T) {
	h, acc, _ := aiHarness(t, false, nil, func(e *coretest.Env) { e.InstallPlugin(t, "card-demo") })
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Invoice 42", "pay me"))
	h.openThread(m.ThreadID)
	h.see("Invoice · 来自插件 card-demo")
	h.see("bob@example.com") // the first field's value, in large mono
	if h.tt.HasText("From") {
		t.Fatal("the first field's label shows")
	}
	h.click("Copy")
	if got := h.tt.Clipboard(); got != "INV-1" {
		t.Fatalf("clipboard %q", got)
	}
	h.see("已复制") // on the button, in place of Copy
	if h.tt.HasText("Copy") {
		t.Fatal("Copy still shows beside 已复制")
	}
	h.advance(1500 * time.Millisecond)
	h.see("Copy")
	h.click("Open")
	h.see("pay.example")
	if !h.app.linkOpen || h.app.link != "https://pay.example/inv" {
		t.Fatalf("link dialog %v %q", h.app.linkOpen, h.app.link)
	}
}

func TestQuickReplies(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noSummary)
	llm.Script(coretest.Turn{Text: []string{`{"replies":[{"html":"<p>Reply one</p>"},{"html":"<p>Reply two</p>"},{"html":"<p>Reply three</p>"}]}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Question", "can you?"))
	h.openThread(m.ThreadID)
	h.see("即时回复 · 选一封，进入撰写页再改")
	h.see("Reply one")
	h.see("Reply two")
	h.see("Reply three")
	for range 200 {
		if h.tt.Focused("Reply two") {
			break
		}
		h.tt.Key(0, ui.KeyTab)
		h.tt.Frame()
	}
	if !h.tt.Focused("Reply two") {
		t.Fatal("Tab never reached the second reply")
	}
	h.tt.Key(0, ui.KeyEnter)
	h.tt.Frame()
	d, err := h.env.Svc.GetDraft(context.Background(), h.composeOpened(0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.HTML, "Reply two") || strings.Contains(d.HTML, "Reply one") || d.SourceMessageID != m.ID {
		t.Fatalf("draft %+v", d)
	}
}

func TestSwitchingCancelsRun(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noQuick)
	llm.Script(coretest.Turn{Hang: true})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Slow", "slow body"))
	other := h.deliverOne(acc, coretest.Msg("carol@example.com", acc.Email, "Other", "other body"))
	r := h.app.mail.reader
	var mu sync.Mutex
	ctxs := map[int64]context.Context{}
	real := r.runSkill
	r.runSkill = func(ctx context.Context, name string, tg core.Target) (<-chan ai.Event, error) {
		mu.Lock()
		ctxs[tg.ID] = ctx
		mu.Unlock()
		return real(ctx, name, tg)
	}
	runCtx := func(id int64) context.Context { mu.Lock(); defer mu.Unlock(); return ctxs[id] }
	h.openThread(m.ThreadID)
	h.until("the summary started", func() bool { return runCtx(m.ID) != nil && llm.Requests() == 1 })
	h.see("m1") // its panel, labelled by its model
	h.openThread(other.ThreadID)
	h.seeBody(other.ID)
	if runCtx(m.ID).Err() == nil {
		t.Fatal("the run of the message left goes on")
	}
	if s := r.ai[m.ID]; s != nil && s.summary != nil {
		t.Fatalf("a cancelled summary kept: %+v", s.summary)
	}
	h.until("the other summary done", func() bool { s := r.ai[other.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.openThread(m.ThreadID)
	h.until("the summary run again", func() bool { s := r.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	if n := llm.Requests(); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}
}

func TestSkillModelLabel(t *testing.T) {
	h, acc, _ := aiHarness(t, true, noQuick, func(e *coretest.Env) {
		ctx := context.Background()
		svcs, err := e.Svc.ListModelServices(ctx)
		if err != nil || len(svcs) != 1 {
			t.Fatalf("services %+v, %v", svcs, err)
		}
		s := svcs[0]
		if _, err := e.Svc.UpdateModelService(ctx, s.ID, core.ModelServiceInput{Name: s.Name, Kind: s.Kind, BaseURL: s.BaseURL, Models: []string{"m1", "m2"}}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(e.DataDir, "skills", "summarize")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := "---\nname: summarize\ndescription: mine\nmetadata:\n  godwit-action-on: \"message\"\n  godwit-action-output: \"panel\"\n  godwit-model: \"test/m2\"\n---\n\nSummarize.\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	r := h.app.mail.reader
	h.openThread(m.ThreadID)
	h.until("the summary done", func() bool { s := r.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.see("m2")
	if h.tt.HasText("m1") {
		t.Fatal("labelled with the default model")
	}
}

func TestOnDemandSummaryCancelled(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noAuto)
	llm.Script(coretest.Turn{Hang: true})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Slow", "slow body"))
	other := h.deliverOne(acc, coretest.Msg("carol@example.com", acc.Email, "Other", "other body"))
	r := h.app.mail.reader
	h.openThread(m.ThreadID)
	h.click("总结")
	h.until("the summary asked", func() bool { return llm.Requests() == 1 })
	h.openThread(other.ThreadID)
	h.seeBody(other.ID)
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	if h.tt.HasText("summarize") {
		t.Fatal("a cancelled summary shown")
	}
	h.click("总结") // one click starts it again
	h.until("the summary done", func() bool { s := r.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.see("m1")
	if n := llm.Requests(); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

// The label is the model the run reports, here the small-task model, not
// the default one.
func TestSummaryLabelFromRun(t *testing.T) {
	h, acc, _ := aiHarness(t, true, func(st *core.Settings) { st.QuickReplies = false; st.SmallModel = "test/m2" }, func(e *coretest.Env) {
		ctx := context.Background()
		svcs, err := e.Svc.ListModelServices(ctx)
		if err != nil || len(svcs) != 1 {
			t.Fatalf("services %+v, %v", svcs, err)
		}
		s := svcs[0]
		if _, err := e.Svc.UpdateModelService(ctx, s.ID, core.ModelServiceInput{Name: s.Name, Kind: s.Kind, BaseURL: s.BaseURL, KeyAction: core.KeyKeep, Models: []string{"m1", "m2"}}); err != nil {
			t.Fatal(err)
		}
	})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	r := h.app.mail.reader
	h.openThread(m.ThreadID)
	h.until("the summary done", func() bool { s := r.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.see("m2")
}

// Final whole-branch review #3: with only a small-task model the reader's
// small tasks run (summary, translate, quick replies); the skill menu still
// needs the default model.
func TestSmallModelOnlyReaderAI(t *testing.T) {
	h, acc, llm := aiHarness(t, true, func(st *core.Settings) { st.DefaultModel, st.SmallModel = "", "test/m1" })
	llm.Script(coretest.Turn{Text: []string{"the summary"}},
		coretest.Turn{Text: []string{`{"replies":[{"html":"<p>Reply one</p>"}]}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	h.openThread(m.ThreadID)
	h.until("the summary done", func() bool { s := h.app.mail.reader.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.see("m1")
	h.see("翻译")
	h.see("Reply one")
	if h.tt.HasText("更多 skill") {
		t.Fatal("the skill menu shows without a default model")
	}
}

// Final re-review R3: the reader asks core which model each task resolves
// to. With no default or small model, a summarize skill naming its own
// model still summarizes.
func TestSummaryBySkillModel(t *testing.T) {
	h, acc, _ := aiHarness(t, true, func(st *core.Settings) { st.DefaultModel, st.SmallModel, st.QuickReplies = "", "", false },
		func(e *coretest.Env) {
			dir := filepath.Join(e.DataDir, "skills", "summarize")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			md := "---\nname: summarize\ndescription: mine\nmetadata:\n  godwit-action-on: \"message\"\n  godwit-action-output: \"panel\"\n  godwit-model: \"test/m1\"\n---\n\nSummarize.\n"
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	r := h.app.mail.reader
	h.openThread(m.ThreadID)
	h.until("the summary done", func() bool { s := r.ai[m.ID]; return s != nil && s.summary != nil && s.summary.done })
	h.see("m1")
	if h.tt.HasText("翻译") {
		t.Fatal("translate shown: it has no model")
	}
}

// Final re-review R3: a small-task model whose model was removed (and no
// default model) resolves nowhere: the small tasks are hidden.
func TestSmallTasksHiddenWhenModelGone(t *testing.T) {
	h, acc, _ := aiHarness(t, true, nil, func(e *coretest.Env) {
		ctx := context.Background()
		svcs, err := e.Svc.ListModelServices(ctx)
		if err != nil || len(svcs) != 1 {
			t.Fatalf("services %+v, %v", svcs, err)
		}
		s := svcs[0]
		in := core.ModelServiceInput{Name: s.Name, Kind: s.Kind, BaseURL: s.BaseURL, Models: []string{"m1", "m2"}}
		if _, err := e.Svc.UpdateModelService(ctx, s.ID, in); err != nil {
			t.Fatal(err)
		}
		st, err := e.Svc.GetSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		st.DefaultModel, st.SmallModel = "", "test/m2"
		if _, err := e.Svc.SetSettings(ctx, st); err != nil {
			t.Fatal(err)
		}
		in.Models = []string{"m1"} // m2 goes; the setting still names it
		if _, err := e.Svc.UpdateModelService(ctx, s.ID, in); err != nil {
			t.Fatal(err)
		}
	})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	for range 20 {
		h.drain()
		h.tt.Frame()
	}
	if h.tt.HasText("总结") || h.tt.HasText("翻译") || h.tt.HasText("即时回复") {
		t.Fatalf("small tasks shown without a model: %q", h.tt.Texts())
	}
	if s := h.app.mail.reader.ai[m.ID]; s != nil && s.summary != nil {
		t.Fatal("a summary started without a model")
	}
}

// A quick reply's draft created after the user switched mail still opens.
func TestQuickReplyAfterSwitchingMailOpensWindow(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noSummary)
	llm.Script(coretest.Turn{Text: []string{`{"replies":[{"html":"<p>Reply one</p>"},{"html":"<p>Reply two</p>"},{"html":"<p>Reply three</p>"}]}`}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Question", "can you?"))
	other := h.deliverOne(acc, coretest.Msg("carol@example.com", acc.Email, "Other", "other body"))
	h.openThread(m.ThreadID)
	h.see("Reply two")
	if err := h.tt.Click("Reply two"); err != nil { // no drain: the draft's callback waits
		t.Fatal(err)
	}
	h.openThread(other.ThreadID)
	if d := h.draft(h.composeOpened(0)); !strings.Contains(d.HTML, "Reply two") || d.SourceMessageID != m.ID {
		t.Fatalf("draft %+v", d)
	}
}

// A plugin action's draft opens in a compose window, also when the user
// switched mail before the action finished.
func TestActionDraftOpensWindow(t *testing.T) {
	h, acc, _ := aiHarness(t, false, nil, func(e *coretest.Env) { e.InstallPlugin(t, "draft-action") })
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Hi", "body words"))
	other := h.deliverOne(acc, coretest.Msg("carol@example.com", acc.Email, "Other", "other body"))
	h.openThread(m.ThreadID)
	h.seeBody(m.ID)
	h.click("更多 skill")
	if err := h.tt.ChooseMenuItem("Draft it"); err != nil { // no drain: the action's callback waits
		t.Fatalf("%v; menu %q", err, h.tt.Menu())
	}
	h.openThread(other.ThreadID)
	if d := h.draft(h.composeOpened(0)); d.Subject != "Re: Hi" || d.AccountID != acc.ID {
		t.Fatalf("draft %+v", d)
	}
}

func TestSummaryHeaderSplit(t *testing.T) {
	h, acc, llm := aiHarness(t, true, noQuick)
	llm.Script(coretest.Turn{Text: []string{"- 第一点要点\n"}})
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "the plan body"))
	h.openThread(m.ThreadID)
	h.see("m1")
	if h.tt.HasText("总结 · summarize") {
		t.Fatalf("one string: %q", h.tt.Texts())
	}
}

func TestCardHint(t *testing.T) {
	got := cardHint([]core.CardField{{Label: "座位", Value: "[待选]"}, {Value: "10 分钟内有效"}})
	if want := "座位 [待选] · 10 分钟内有效"; got != want {
		t.Errorf("cardHint = %q, want %q", got, want)
	}
}

// mygo ends a bare URL at CJK punctuation (#402).
func TestSummaryAutolinkEndsAtCJK(t *testing.T) {
	var dests []string
	for _, in := range parser.Parse("链接：https://vishnuai.in/mediakit，并留下电话。").Inlines(1) { // block 0 is the root
		if in.Kind == parser.Link {
			dests = append(dests, in.Dest)
		}
	}
	if !slices.Equal(dests, []string{"https://vishnuai.in/mediakit"}) {
		t.Errorf("links = %q", dests)
	}
}

// The reader's 引用给 AI hands the message read, quoted, to a new Wit
// conversation, with a first question about it.
func TestReaderQuoteToWit(t *testing.T) {
	h, acc, _ := aiHarness(t, false, func(st *core.Settings) { st.DefaultModel = "test/m1" })
	var asked string
	var quoted []core.MessageSummary
	h.app.SetWitAsk(func(q string, ms ...core.MessageSummary) { asked, quoted = q, ms })
	m := h.deliverOne(acc, coretest.Msg("bob@example.com", acc.Email, "Plan", "first words"))
	h.app.OpenMessage(m.ID)
	h.see(i18n.T("reader.ai.quote"))

	h.click(i18n.T("reader.ai.quote"))

	if h.app.route.Page != PageWit {
		t.Fatalf("route %+v, want the Wit page", h.app.route)
	}
	if asked != i18n.T("reader.ai.quote_ask") {
		t.Fatalf("asked %q, want %q", asked, i18n.T("reader.ai.quote_ask"))
	}
	if len(quoted) != 1 || quoted[0].ID != m.ID {
		t.Fatalf("quoted %+v, want message %d", quoted, m.ID)
	}
}
