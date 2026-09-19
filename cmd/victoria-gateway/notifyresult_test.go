package main

import (
	"testing"

	"github.com/gordonwei/victoria-gateway/pkg/notify"
)

// fakeNotifyChannel records every Message it was asked to send, so a test
// can assert what notifyResult actually built without a real Telegram or
// webhook round trip.
type fakeNotifyChannel struct {
	name string
	sent []notify.Message
}

func (f *fakeNotifyChannel) Name() string { return f.name }
func (f *fakeNotifyChannel) Send(msg notify.Message) error {
	f.sent = append(f.sent, msg)
	return nil
}

func newTestRouter(t *testing.T, ch *fakeNotifyChannel) *notify.Router {
	t.Helper()
	r, err := notify.NewRouter([]notify.Channel{ch}, []notify.Route{{Default: true, Channels: []string{ch.name}}}, nil)
	if err != nil {
		t.Fatalf("notify.NewRouter: %v", err)
	}
	return r
}

func TestNotifyResult_PendingID_BuildsPendingURL(t *testing.T) {
	ch := &fakeNotifyChannel{name: "test"}
	h := &handler{notifier: newTestRouter(t, ch), publicBaseURL: "https://vg.example"}

	h.notifyResult(alertResult{AlertName: "DiskSpace", Host: "h1", pendingID: 42}, nil)

	if len(ch.sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(ch.sent))
	}
	if want := "https://vg.example/pending/42"; ch.sent[0].PendingURL != want {
		t.Errorf("PendingURL = %q, want %q", ch.sent[0].PendingURL, want)
	}
}

func TestNotifyResult_NoPendingID_NoPendingURL(t *testing.T) {
	ch := &fakeNotifyChannel{name: "test"}
	h := &handler{notifier: newTestRouter(t, ch), publicBaseURL: "https://vg.example"}

	h.notifyResult(alertResult{AlertName: "DiskSpace", Host: "h1"}, nil) // pendingID left at zero (RAG off or capture failed)

	if len(ch.sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(ch.sent))
	}
	if ch.sent[0].PendingURL != "" {
		t.Errorf("expected empty PendingURL when pendingID is 0, got %q", ch.sent[0].PendingURL)
	}
}

func TestNotifyResult_MutedResult_NotDispatched(t *testing.T) {
	ch := &fakeNotifyChannel{name: "test"}
	h := &handler{notifier: newTestRouter(t, ch), publicBaseURL: "https://vg.example"}

	h.notifyResult(alertResult{AlertName: "DiskSpace", Host: "h1", pendingID: 42, muted: true}, nil)

	if len(ch.sent) != 0 {
		t.Errorf("a muted result must not be dispatched, got %d messages", len(ch.sent))
	}
}
