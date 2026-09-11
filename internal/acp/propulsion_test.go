package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
)

func TestNewPropeller(t *testing.T) {
	proxy := NewProxy()
	prop := NewPropeller(proxy, "/town", "hq-mayor")

	if prop.proxy != proxy {
		t.Error("proxy not set correctly")
	}
	if prop.townRoot != "/town" {
		t.Error("townRoot not set correctly")
	}
	if prop.session != "hq-mayor" {
		t.Error("session not set correctly")
	}
}

func TestPropeller_StartStop(t *testing.T) {
	prop := NewPropeller(nil, "", "hq-mayor")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prop.Start(ctx)

	time.Sleep(100 * time.Millisecond)

	prop.Stop()
}

func TestPropeller_DeliverNudges_NoProxy(t *testing.T) {
	// Test that deliverNudges handles nil proxy gracefully
	prop := NewPropeller(nil, "/town", "hq-mayor")
	prop.deliverNudges() // Should not panic
}

func TestPropeller_EventLoop_Cancellation(t *testing.T) {
	// Test that eventLoop exits on context cancellation
	prop := NewPropeller(nil, "/town", "hq-mayor")

	ctx, cancel := context.WithCancel(context.Background())
	prop.ctx = ctx
	prop.cancel = cancel

	// Start eventLoop in a goroutine
	done := make(chan struct{})
	go func() {
		prop.eventLoop()
		close(done)
	}()

	// Cancel context
	cancel()

	// Wait for eventLoop to exit
	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Error("eventLoop did not exit after context cancellation")
	}
}

func TestPropeller_DeliverNudges_RequeuesWhenSessionUnavailable(t *testing.T) {
	townRoot := t.TempDir()
	proxy := NewProxy()
	prop := NewPropeller(proxy, townRoot, "hq-mayor")

	if err := nudge.Enqueue(townRoot, "hq-mayor", nudge.QueuedNudge{
		Sender:   "witness",
		Message:  "Escalation pending",
		Priority: nudge.PriorityUrgent,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	prop.deliverNudges()

	pending, err := nudge.Pending(townRoot, "hq-mayor")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("expected requeued nudge to remain pending, got %d", pending)
	}

	drained, err := nudge.Drain(townRoot, "hq-mayor")
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(drained) != 1 {
		t.Fatalf("expected 1 requeued nudge, got %d", len(drained))
	}
	if drained[0].Priority != nudge.PriorityUrgent {
		t.Fatalf("priority = %q, want %q", drained[0].Priority, nudge.PriorityUrgent)
	}
}

// TestPropeller_DeliverNudges_RetainsClaimWhenBusy covers High 6
// (propulsion.go:267): a busy, non-urgent session must NOT have its claim
// acked on the strength of notifyWithMeta's UI-only notification alone —
// InjectPrompt is the only channel that actually delivers content to the
// agent's turn, and it is deliberately skipped while busy.
func TestPropeller_DeliverNudges_RetainsClaimWhenBusy(t *testing.T) {
	townRoot := t.TempDir()
	session := "hq-mayor"
	proxy := NewProxy()
	proxy.setStreams(bytes.NewReader(nil), &bytes.Buffer{})
	proxy.sessionID = "sess-busy"
	proxy.activePromptID = "in-flight-prompt"

	prop := NewPropeller(proxy, townRoot, session)

	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		ID: "busy-1", Sender: "witness", Message: "normal-priority nudge",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	prop.deliverNudges()

	// Not acked: the claim must still exist on disk (as a .claimed file,
	// not requeued as a fresh .json — see the no-hot-loop comment in
	// deliverNudges).
	has, err := nudge.PendingOrClaimed(townRoot, session)
	if err != nil {
		t.Fatalf("PendingOrClaimed: %v", err)
	}
	if !has {
		t.Fatal("claim was lost (acked) while the session was busy — deliverNudges must retain it, not ack on a UI-only notification")
	}
	if pending, _ := nudge.Pending(townRoot, session); pending != 0 {
		t.Fatalf("Pending = %d, want 0: a retained claim must not be a fresh requeued .json (that risks a hot loop while busy)", pending)
	}
}

// TestPropeller_EventLoop_PeriodicRecoveryTicker covers High 7
// (propulsion.go:190): a retained claim (see the busy test above) produces
// no filesystem event of its own, so without a periodic sweep it has no
// recovery trigger at all. This proves eventLoop calls deliverNudges on the
// ticker, not only on watcher.Events() — it goes RED if the ticker case is
// removed from eventLoop's select.
func TestPropeller_EventLoop_PeriodicRecoveryTicker(t *testing.T) {
	orig := claimRecoveryInterval
	claimRecoveryInterval = 20 * time.Millisecond
	t.Cleanup(func() { claimRecoveryInterval = orig })

	townRoot := t.TempDir()
	session := "hq-mayor-ticker"
	proxy := NewProxy()
	proxy.setStreams(bytes.NewReader(nil), &bytes.Buffer{})
	// No sessionID: deliverNudges takes the "session not ready" requeue
	// path, which is enough to observe whether it ran at all — this test
	// is about the TRIGGER (does the ticker fire deliverNudges), not the
	// delivery outcome (covered separately above).

	// Enqueue BEFORE eventLoop (and its watcher) start: fsnotify only
	// reports events for filesystem activity that happens AFTER a watch
	// begins, so a file that already exists when the watcher attaches
	// produces no "create" event. With no other write to the queue
	// directory after this point, watcher.Events() can never fire for
	// this entry — the ONLY thing that can ever drain and requeue it is
	// the periodic ticker.
	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		ID: "ticker-1", Sender: "witness", Message: "hello",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	prop := NewPropeller(proxy, townRoot, session)
	ctx, cancel := context.WithCancel(context.Background())
	prop.ctx = ctx
	prop.cancel = cancel

	done := make(chan struct{})
	go func() {
		prop.eventLoop()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The claim has no sessionID to deliver to, so deliverNudges requeues
	// it — Requeue writes a fresh pending .json file. That can only
	// happen if the ticker actually invoked deliverNudges, since nothing
	// else in this test drives the watcher.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending, _ := nudge.Pending(townRoot, session); pending == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("claim was never requeued — the periodic recovery ticker did not fire deliverNudges")
}

func TestPropeller_NotifyReturnsErrorWithoutSessionID(t *testing.T) {
	proxy := NewProxy()
	prop := NewPropeller(proxy, t.TempDir(), "hq-mayor")

	err := prop.notify("test message", map[string]string{"gt/eventType": "nudge"}, true)
	if err == nil {
		t.Fatal("expected notify to fail when sessionID is unavailable")
	}
}

func TestEscalationMetaFromNudges_MetadataDriven(t *testing.T) {
	nudges := []nudge.QueuedNudge{
		{Sender: "witness", Message: "Helpful document about urgent migrations", Priority: nudge.PriorityNormal, Kind: "mail"},
		{Sender: "witness", Message: "Neutral text", Priority: nudge.PriorityUrgent, Kind: "escalation", ThreadID: "hq-esc789", Severity: "critical"},
	}

	meta := escalationMetaFromNudges(nudges)
	if meta == nil {
		t.Fatal("expected escalation metadata")
	}
	if meta.Kind != "escalation" || meta.ThreadID != "hq-esc789" || meta.Severity != "critical" {
		t.Fatalf("unexpected escalation meta: %#v", meta)
	}
}

func TestEscalationMetaFromNudges_IgnoresHeuristicText(t *testing.T) {
	nudges := []nudge.QueuedNudge{
		{Sender: "witness", Message: "Urgent migration doc that is helpful", Priority: nudge.PriorityUrgent, Kind: "mail"},
	}

	if meta := escalationMetaFromNudges(nudges); meta != nil {
		t.Fatalf("expected no escalation metadata for generic mail, got %#v", meta)
	}
}

func TestBuildSessionUpdateMetaAddsEscalationFields(t *testing.T) {
	nudges := []nudge.QueuedNudge{{Sender: "witness", Message: "neutral", Priority: nudge.PriorityUrgent, Kind: "escalation", ThreadID: "hq-esc999", Severity: "critical"}}
	meta := buildSessionUpdateMeta(nudges, "hq-mayor")
	if meta["gt/escalation"] != "true" || meta["gt/threadID"] != "hq-esc999" || meta["gt/severity"] != "critical" || meta["gt/kind"] != "escalation" {
		t.Fatalf("unexpected meta: %#v", meta)
	}
}

func TestNotifyWithMetaInjectsEscalationMetadataToUI(t *testing.T) {
	p := NewProxy()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	defer r.Close()
	p.setStreams(nil, w)
	p.sessionMux.Lock()
	p.sessionID = "test-session"
	p.sessionMux.Unlock()

	prop := NewPropeller(p, t.TempDir(), "hq-mayor")
	meta := map[string]string{"gt/eventType": "nudge", "gt/escalation": "true", "gt/threadID": "hq-esc777", "gt/severity": "high"}

	go func() {
		prop.notifyWithMeta("Escalation text", meta)
		w.Close()
	}()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("failed to read pipe: %v", err)
	}

	var msg JSONRPCMessage
	if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
		t.Fatalf("failed to parse message: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		t.Fatalf("failed to parse params: %v", err)
	}
	update := params["update"].(map[string]any)
	metaAny := update["_meta"].(map[string]any)
	if metaAny["gt/escalation"] != "true" || metaAny["gt/threadID"] != "hq-esc777" || metaAny["gt/severity"] != "high" {
		t.Fatalf("unexpected injected _meta: %#v", metaAny)
	}
}

func TestACPAttachedMayorEscalationPath_MetadataAndUrgencyEndToEnd(t *testing.T) {
	townRoot := t.TempDir()
	if err := nudge.Enqueue(townRoot, "hq-mayor", nudge.QueuedNudge{
		Sender:   "gastown/witness",
		Message:  "Escalation mail from gastown/witness. ID: hq-esc-end2end. Severity: critical. Run 'gt mail read hq-esc-end2end' or 'gt escalate ack hq-esc-end2end'.",
		Priority: nudge.PriorityUrgent,
		Kind:     "escalation",
		ThreadID: "hq-esc-end2end",
		Severity: "critical",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	p := NewProxy()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer r.Close()
	p.setStreams(nil, w)
	p.sessionMux.Lock()
	p.sessionID = "attached-session"
	p.sessionMux.Unlock()

	prop := NewPropeller(p, townRoot, "hq-mayor")
	go func() {
		prop.deliverNudges()
		w.Close()
	}()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	var msg JSONRPCMessage
	if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
		t.Fatalf("json.Unmarshal message: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		t.Fatalf("json.Unmarshal params: %v", err)
	}
	update := params["update"].(map[string]any)
	content, ok := update["content"].(map[string]any)
	if !ok {
		t.Fatalf("expected structured content payload, got %#v", update["content"])
	}
	if text, ok := content["text"].(string); !ok || !bytes.Contains([]byte(text), []byte("hq-esc-end2end")) {
		t.Fatalf("session/update content missing escalation id: %#v", content)
	}
	meta := update["_meta"].(map[string]any)
	for key, want := range map[string]string{"gt/escalation": "true", "gt/threadID": "hq-esc-end2end", "gt/severity": "critical", "gt/kind": "escalation", "gt/urgent": "1"} {
		if meta[key] != want {
			t.Fatalf("_meta[%s] = %#v, want %q", key, meta[key], want)
		}
	}
	remaining, err := nudge.Pending(townRoot, "hq-mayor")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("expected only deferred prompt reminder to remain after successful attached delivery, got %d", remaining)
	}
}
