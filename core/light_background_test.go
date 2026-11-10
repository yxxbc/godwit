package core

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/mailsync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A light round fetches the inbox and stops there: the folders it skips come
// with the next full round.
func TestFetchInboxOnly(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", eml("in@x", "Bob <bob@example.com>", "alice@example.com", "inbox", "Mon, 05 Oct 2026 09:00:00 +0000", "new mail"))
	e.put(t, "Archive", eml("ar@x", "Bob <bob@example.com>", "alice@example.com", "archived", "Mon, 05 Oct 2026 09:05:00 +0000", "older mail"))

	c := e.dial(t)
	_, err := mailsync.Fetch(bg, e.s.db, c, e.acc.ID, e.s.bus, mailsync.FetchOptions{InboxOnly: true})
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages"))
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE message_id = 'in@x'"))

	_, err = mailsync.Fetch(bg, e.s.db, c, e.acc.ID, e.s.bus, mailsync.FetchOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE message_id = 'ar@x'"))
}

// SyncNow stays whole while the background is light: a sync the user asks for
// is not narrowed.
func TestLightBackgroundKeepsSyncNowWhole(t *testing.T) {
	e := newSyncEnv(t)
	e.s.SetLightBackground(true)
	defer e.s.SetLightBackground(false)
	e.put(t, "Archive", eml("ar@x", "Bob <bob@example.com>", "alice@example.com", "archived", "Mon, 05 Oct 2026 09:05:00 +0000", "older mail"))

	e.sync(t)

	assert.True(t, e.s.LightBackground())
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE message_id = 'ar@x'"))
}

// The worker's rounds follow the mode: light, they carry the inbox only, and
// turning it off wakes them into a full round.
func TestWorkerLightBackgroundSyncsInboxOnly(t *testing.T) {
	e, ch := newWorkerEnv(t)
	e.s.SetLightBackground(true)
	e.put(t, "INBOX", eml("in@x", "Bob <bob@example.com>", "alice@example.com", "inbox", "Mon, 05 Oct 2026 09:00:00 +0000", "new mail"))
	e.put(t, "Archive", eml("ar@x", "Bob <bob@example.com>", "alice@example.com", "archived", "Mon, 05 Oct 2026 09:05:00 +0000", "older mail"))

	e.s.wakeWorker(e.acc.ID)
	waitEvent(t, ch, events.SyncFinished, 5*time.Second)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE message_id = 'in@x'"))
	assert.Zero(t, e.count(t, "SELECT count(*) FROM messages WHERE message_id = 'ar@x'"))

	e.s.SetLightBackground(false)
	waitEvent(t, ch, events.SyncFinished, 5*time.Second)
	assert.Equal(t, 1, e.count(t, "SELECT count(*) FROM messages WHERE message_id = 'ar@x'"))
}

// roundCost is what one sync round took: wall time, bytes the server sent and
// bytes the process allocated.
type roundCost struct {
	took        time.Duration
	sent, alloc uint64
}

// A background round's cost is what a window behind another app pays every few
// minutes. The light round reads the inbox alone, so the server sends it a
// fraction of the bytes of a round that reads every folder.
func TestBackgroundRoundCost(t *testing.T) {
	const folders, perFolder = 20, 20
	e := newSyncEnv(t)
	for i := range folders {
		name := fmt.Sprintf("Projects/%02d", i+1)
		require.NoError(t, e.im.User.Create(name, &imap.CreateOptions{}))
		for j := range perFolder {
			e.put(t, name, eml(fmt.Sprintf("f%d-%d@x", i, j), "Bob <bob@example.com>", "alice@example.com",
				fmt.Sprintf("folder %d message %d", i, j), "Mon, 05 Oct 2026 09:00:00 +0000", "body"))
		}
	}
	for j := range perFolder {
		e.put(t, "INBOX", eml(fmt.Sprintf("in-%d@x", j), "Bob <bob@example.com>", "alice@example.com",
			fmt.Sprintf("inbox message %d", j), "Mon, 05 Oct 2026 09:00:00 +0000", "body"))
	}

	c := e.dial(t)
	round := func(opts mailsync.FetchOptions) roundCost {
		t.Helper()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		sent := e.im.BytesSent()
		start := time.Now()
		_, err := mailsync.Fetch(bg, e.s.db, c, e.acc.ID, e.s.bus, opts)
		took := time.Since(start)
		require.NoError(t, err)
		runtime.ReadMemStats(&after)
		return roundCost{took, uint64(e.im.BytesSent() - sent), after.TotalAlloc - before.TotalAlloc}
	}

	round(mailsync.FetchOptions{}) // the first sync downloads every folder
	full := round(mailsync.FetchOptions{})
	light := round(mailsync.FetchOptions{InboxOnly: true})
	t.Logf("%d folders, %d messages: full round %v, %d KiB from the server, %d KiB allocated; inbox only %v, %d KiB, %d KiB",
		folders+1, (folders+1)*perFolder, full.took, full.sent>>10, full.alloc>>10, light.took, light.sent>>10, light.alloc>>10)
	assert.Less(t, light.sent*4, full.sent, "the light round sent %d bytes, the full round %d", light.sent, full.sent)
}
