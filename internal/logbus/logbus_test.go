package logbus

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/models"
)

func TestSnapshotPlusStreamIsExact(t *testing.T) {
	b := New()
	b.Publish(1, []byte("early "))

	snap, cur, ch, unsub := b.Subscribe(1, 0)
	defer unsub()
	if string(snap) != "early " || cur != 6 {
		t.Fatalf("snapshot = %q cur=%d", snap, cur)
	}

	b.Publish(1, []byte("late"))
	ev := <-ch
	if ev.Kind != "log" || ev.Chunk != "late" || ev.Offset != 10 {
		t.Errorf("event = %+v", ev)
	}
	if got := string(snap) + ev.Chunk; got != "early late" {
		t.Errorf("reassembled = %q", got)
	}
}

func TestSubscribeFromOffset(t *testing.T) {
	b := New()
	b.Publish(1, []byte("0123456789"))

	snap, cur, _, unsub := b.Subscribe(1, 4)
	defer unsub()
	if string(snap) != "456789" || cur != 10 {
		t.Errorf("snap=%q cur=%d", snap, cur)
	}

	// Offset beyond buffer clamps to empty, not panic.
	snap, cur, _, unsub2 := b.Subscribe(1, 99)
	defer unsub2()
	if len(snap) != 0 || cur != 10 {
		t.Errorf("beyond-end snap=%q cur=%d", snap, cur)
	}
}

// Concurrent publishes with a subscriber attaching mid-stream must yield a
// gapless, duplicate-free byte sequence when snapshot + events are combined.
func TestSubscribeDuringPublishNoGapsNoDupes(t *testing.T) {
	b := New()
	const n = 200

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			b.Publish(1, []byte("x"))
		}
		now := time.Now()
		b.PublishStatus(1, models.StatusSuccess, &now, &now)
	}()

	// Subscribe at a random point mid-publish.
	time.Sleep(time.Millisecond)
	snap, _, ch, unsub := b.Subscribe(1, 0)
	defer unsub()

	var sb strings.Builder
	sb.Write(snap)
	for ev := range ch {
		if ev.Kind == "log" {
			sb.WriteString(ev.Chunk)
			// Offset must equal total bytes seen so far.
			if ev.Offset != sb.Len() {
				t.Fatalf("offset %d != assembled length %d", ev.Offset, sb.Len())
			}
		}
	}
	wg.Wait()
	if sb.Len() != n {
		t.Errorf("assembled %d bytes, want %d", sb.Len(), n)
	}
}

func TestTerminalStatusClosesSubscribers(t *testing.T) {
	b := New()
	_, _, ch, unsub := b.Subscribe(1, 0)
	defer unsub()

	now := time.Now()
	b.PublishStatus(1, models.StatusFailed, &now, &now)

	ev, open := <-ch
	if !open || ev.Kind != "status" || ev.Status != models.StatusFailed {
		t.Fatalf("expected status event, got %+v open=%v", ev, open)
	}
	if _, open := <-ch; open {
		t.Error("channel not closed after terminal status")
	}

	// Publishing after close is a no-op; the buffer stays readable.
	b.Publish(1, []byte("ignored"))
	tail, cur, ok := b.LogTail(1, 0)
	if !ok || cur != 0 || len(tail) != 0 {
		t.Errorf("post-close tail=%q cur=%d ok=%v", tail, cur, ok)
	}
	if b.Live(1) {
		t.Error("closed topic reported live")
	}

	// Late subscriber gets snapshot + already-closed channel.
	_, _, ch2, _ := b.Subscribe(1, 0)
	if _, open := <-ch2; open {
		t.Error("late subscriber channel should be closed immediately")
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	b := New()
	_, _, ch, unsub := b.Subscribe(1, 0)
	defer unsub()

	// Overflow the subscriber buffer without reading.
	for i := 0; i < subBuffer+10; i++ {
		b.Publish(1, []byte("y"))
	}

	// Drain: channel must be closed (dropped), not blocked.
	count := 0
	for range ch {
		count++
	}
	if count != subBuffer {
		t.Errorf("drained %d events, want %d then close", count, subBuffer)
	}
	// The full log is still recoverable via LogTail.
	tail, _, ok := b.LogTail(1, 0)
	if !ok || len(tail) != subBuffer+10 {
		t.Errorf("tail = %d bytes, want %d", len(tail), subBuffer+10)
	}
}

func TestLogTailUnknownBuild(t *testing.T) {
	b := New()
	if _, _, ok := b.LogTail(42, 0); ok {
		t.Error("unknown build should report ok=false")
	}
}
