package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/townlog"
)

// errBusyDeferred signals that notify deliberately skipped InjectPrompt this
// cycle (busy agent, non-urgent nudge) rather than attempting and failing
// it. deliverNudges must not treat this like a successful delivery: only
// notifyWithMeta's UI-only side channel saw the content, and the agent's
// active turn never received it via InjectPrompt — acking the claim here
// would delete it on the strength of that UI notification alone (codex,
// propulsion.go:267, changes-requested at REVISION 3 — High 6).
var errBusyDeferred = errors.New("nudge delivery deferred: session busy")

// claimRecoveryInterval periodically re-invokes deliverNudges even when no
// new nudge arrives. A retained claim (busy-deferred delivery, or a
// requeue/dead-letter write that itself failed) produces no filesystem
// event of its own — WatcherForSession only fires on a fresh .json file —
// so without this, such a claim has no recovery trigger and can sit until
// an unrelated new nudge happens to arrive (codex, propulsion.go:190,
// changes-requested at REVISION 3 — High 7). Deliberately shorter than
// nudge's staleClaimThreshold (5 minutes) so a busy-deferred claim gets
// re-checked well before it would otherwise be swept as orphaned.
const claimRecoveryInterval = 60 * time.Second

// acpDebugLogger provides file-based debug logging for ACP when GT_ACP_DEBUG=1.
// It lazily opens the log file on first use and keeps it open for the session.
type acpDebugLogger struct {
	mu       sync.Mutex
	file     *os.File
	townRoot string
	enabled  bool
}

var debugLogger = &acpDebugLogger{
	enabled: os.Getenv("GT_ACP_DEBUG") != "",
}

// init opens the log file if debugging is enabled. It is called lazily
// when the first debug log is written, with the townRoot context.
func (l *acpDebugLogger) init(townRoot string) error {
	if !l.enabled {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		return nil
	}

	if townRoot == "" {
		return fmt.Errorf("townRoot is empty")
	}

	logDir := filepath.Join(townRoot, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("creating logs directory: %w", err)
	}

	logPath := filepath.Join(logDir, "acp.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening acp.log: %w", err)
	}

	l.file = f
	l.townRoot = townRoot
	return nil
}

// log writes a debug message to the ACP log file if debugging is enabled.
func (l *acpDebugLogger) log(format string, args ...any) {
	if !l.enabled {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.file, "%s %s\n", timestamp, msg)
}

// close closes the log file if it was opened.
func (l *acpDebugLogger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// debugLog logs to acp.log when GT_ACP_DEBUG=1. It lazily initializes
// the log file on first call with the given townRoot.
func debugLog(townRoot, format string, args ...any) {
	if !debugLogger.enabled {
		return
	}
	// Lazy init - only creates file on first debug log
	if debugLogger.file == nil {
		if err := debugLogger.init(townRoot); err != nil {
			// Can't log anywhere useful if file init fails, silently return
			return
		}
	}
	debugLogger.log(format, args...)
}

// logEvent logs an important event to town.log (failures, errors, lifecycle).
func logEvent(townRoot, eventType, context string) {
	if townRoot == "" {
		return
	}
	logger := townlog.NewLogger(townRoot)
	_ = logger.Log(townlog.EventType(eventType), "mayor/acp", context)
	// Also log to acp.log if debug mode is enabled
	debugLog(townRoot, "[%s] %s", eventType, context)
}

type Propeller struct {
	proxy       *Proxy
	townRoot    string
	session     string
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	warnedNoSID bool
}

func NewPropeller(proxy *Proxy, townRoot, session string) *Propeller {
	return &Propeller{
		proxy:    proxy,
		townRoot: townRoot,
		session:  session,
	}
}

func (p *Propeller) Start(ctx context.Context) {
	debugLog(p.townRoot, "[Propeller] Starting for session %q in town %q", p.session, p.townRoot)
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go p.waitForSessionAndStart()
}

// waitForSessionAndPoll waits for the ACP handshake to complete (sessionID available)
// before starting the poll loop. This ensures the proxy is ready to inject prompts.
// waitForSessionAndStart waits for the ACP handshake to complete (sessionID available)
// before starting the event-driven loop. This ensures the proxy is ready to inject prompts.
// If no IDE connects within the timeout, event loop continues with degraded functionality
// (notifications will be skipped since there's no session to inject into).
func (p *Propeller) waitForSessionAndStart() {
	defer p.wg.Done()

	// Wait for sessionID with a timeout
	waitCtx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	if p.proxy != nil {
		if err := p.proxy.WaitForSessionID(waitCtx); err != nil {
			// Log to town.log - this is a significant event (degraded mode)
			logEvent(p.townRoot, "acp_degraded", "sessionID not available: no IDE connected, notifications disabled")
			debugLog(p.townRoot, "[Propeller] SessionID not available after 30s: %v", err)
			debugLog(p.townRoot, "[Propeller] Continuing with degraded mode - mail/hook detection will work but notifications will be skipped")
			debugLog(p.townRoot, "[Propeller] This is expected if no ACP client (IDE) is connected to the proxy")
		} else {
			debugLog(p.townRoot, "[Propeller] SessionID available, starting event-driven loop with full notification support")
		}
	}

	p.eventLoop()
}

// eventLoop drives event-driven propulsion by listening to the nudge queue watcher.
// It drains queued nudges and injects them immediately when the watcher signals.
func (p *Propeller) eventLoop() {
	// Create watcher for the nudge queue
	watcher, err := nudge.WatcherForSession(p.townRoot, p.session)
	if err != nil {
		debugLog(p.townRoot, "[Propeller] Failed to create nudge watcher: %v", err)
		// If we can't watch, we can't deliver nudges. Log and exit.
		logEvent(p.townRoot, "acp_error", fmt.Sprintf("failed to create nudge watcher: %v", err))
		return
	}
	defer func() { _ = watcher.Close() }()

	ticker := time.NewTicker(claimRecoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-watcher.Events():
			p.deliverNudges()
		case <-ticker.C:
			// Periodic recovery sweep — see claimRecoveryInterval.
			p.deliverNudges()
		}
	}
}

// deliverNudges drains queued nudges and injects them into the ACP session.
//
// Uses DrainClaims, not Drain: Drain deletes each entry from the queue the
// instant it is read, before p.notify (the fallible ACP delivery call
// below) is even attempted. p.notify DOES fail in practice — the requeue
// fallback below exists precisely because it does — so a crash between
// Drain's delete and a subsequent Requeue call lost the entry with no
// durable trace anywhere, the same ordering bug nudge_poller.go's claim-
// based rework closed for tmux delivery (codex, propulsion.go:198,235,
// changes-requested at 08964387/95f841e6 rework: the PR that introduced
// nudge.Claim/DrainClaims had claimed, incorrectly, that this caller "cannot
// fail the way a tmux send can"). Claims are Acked only once the outcome —
// successful delivery, or a durable requeue write — is itself durable.
func (p *Propeller) deliverNudges() {
	claims, err := nudge.DrainClaims(p.townRoot, p.session)
	if err != nil {
		debugLog(p.townRoot, "[Propeller] deliverNudges: DrainClaims error: %v", err)
		return
	}
	if len(claims) == 0 {
		return
	}
	nudges := make([]nudge.QueuedNudge, len(claims))
	for i, c := range claims {
		nudges[i] = c.Nudge
	}

	debugLog(p.townRoot, "[Propeller] deliverNudges: drained %d nudge(s)", len(nudges))

	text := formatNudgesForPropeller(nudges)

	// Determine urgency
	urgent := false
	for _, n := range nudges {
		if n.Priority == nudge.PriorityUrgent {
			urgent = true
			break
		}
	}

	meta := buildSessionUpdateMeta(nudges, p.session)
	requeue := func(reason string) {
		failed, err := nudge.Requeue(p.townRoot, p.session, nudges)
		if err != nil {
			logEvent(p.townRoot, "acp_error", fmt.Sprintf("failed to requeue %d/%d nudges after %s: %v", len(failed), len(nudges), reason, err))
			style.PrintWarning("ACP Propeller failed to requeue %d/%d nudges after %s: %v", len(failed), len(nudges), reason, err)
		} else {
			logEvent(p.townRoot, "acp_degraded", fmt.Sprintf("requeued %d nudges: %s", len(nudges), reason))
		}
		// Ack every claim whose requeue write succeeded. An entry in
		// failed did NOT durably land anywhere — its claim is left
		// un-acked so a future DrainClaims orphan sweep can recover it
		// instead of losing it here.
		nudge.AckClaims(claims, failed, func(c nudge.Claim, ackErr error) {
			style.PrintWarning("ACP Propeller failed to ack nudge claim for %s: %v", c.Nudge.ID, ackErr)
		})
	}

	if p.proxy == nil || p.proxy.SessionID() == "" {
		requeue("session not ready")
		return
	}

	if err := p.notify(text, meta, urgent); err != nil {
		if errors.Is(err, errBusyDeferred) {
			// Busy, non-urgent: leave every claim retained (un-acked)
			// rather than requeuing. Requeuing would write a fresh .json
			// file immediately, which the watcher would see right away —
			// if the agent stays busy, that becomes a tight requeue/re-
			// notify loop. Leaving the claim as-is means the next
			// recovery trigger (a new nudge, or claimRecoveryInterval)
			// picks it up via DrainClaims' own orphan sweep once
			// staleClaimThreshold passes. Acking here would delete the
			// claim on nothing more than a UI-only notification (High 6).
			debugLog(p.townRoot, "[Propeller] deliverNudges: session busy, retaining %d claim(s) for later recovery", len(claims))
			return
		}
		requeue(fmt.Sprintf("delivery failure: %v", err))
		style.PrintWarning("ACP Propeller failed to deliver nudge: %v", err)
		return
	}

	// Delivery succeeded — every claim's outcome is now durable (the agent
	// received it), so ack them all.
	nudge.AckClaims(claims, nil, func(c nudge.Claim, ackErr error) {
		style.PrintWarning("ACP Propeller failed to ack nudge claim for %s: %v", c.Nudge.ID, ackErr)
	})
}

type escalationDeliveryMeta struct {
	Kind     string
	ThreadID string
	Severity string
}

func escalationMetaFromNudges(nudges []nudge.QueuedNudge) *escalationDeliveryMeta {
	for _, n := range nudges {
		if n.Kind != "escalation" {
			continue
		}
		return &escalationDeliveryMeta{
			Kind:     n.Kind,
			ThreadID: n.ThreadID,
			Severity: n.Severity,
		}
	}
	return nil
}

func formatNudgesForPropeller(nudges []nudge.QueuedNudge) string {
	return nudge.FormatForInjection(nudges)
}

func buildSessionUpdateMeta(nudges []nudge.QueuedNudge, session string) map[string]string {
	meta := map[string]string{
		"gt/eventType": "nudge",
		"gt/count":     strconv.Itoa(len(nudges)),
		"gt/drained":   "true",
		"gt/session":   session,
	}
	urgentCount := 0
	for _, n := range nudges {
		if n.Priority == nudge.PriorityUrgent {
			urgentCount++
		}
	}
	meta["gt/urgent"] = strconv.Itoa(urgentCount)
	if escalationMeta := escalationMetaFromNudges(nudges); escalationMeta != nil {
		meta["gt/escalation"] = "true"
		meta["gt/threadID"] = escalationMeta.ThreadID
		meta["gt/severity"] = escalationMeta.Severity
		meta["gt/kind"] = escalationMeta.Kind
	}
	return meta
}

func (p *Propeller) Stop() {
	debugLog(p.townRoot, "[Propeller] Stopping")
	// Close the debug log file if it was opened
	debugLogger.close()
	logEvent(p.townRoot, "acp_stop", "propeller stopped")
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *Propeller) notifyWithMeta(text string, meta map[string]string) {
	if p.proxy == nil || text == "" {
		return
	}

	sessionID := p.proxy.SessionID()
	if sessionID == "" {
		// Log once when sessionID is not available (e.g., no IDE connected to ACP proxy)
		if !p.warnedNoSID {
			p.warnedNoSID = true
			debugLog(p.townRoot, "[Propeller] notifyWithMeta: sessionID not available - ACP handshake may not have completed (no IDE connected?). Notifications will be skipped.")
		}
		return
	}

	debugLog(p.townRoot, "[Propeller] notifyWithMeta: sessionID=%q text=%q", sessionID, text)

	params := map[string]any{
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content": map[string]any{
				"type": "text",
				"text": "\n\n" + text + "\n\n",
			},
			"_meta": meta,
		},
	}

	if err := p.proxy.InjectNotificationToUI("session/update", params); err != nil {
		style.PrintWarning("ACP Propeller failed to inject notification: %v", err)
	}
}

// notify sends a notification to both the UI (via session/update) and the Agent (via session/prompt).
// This couples the two operations, ensuring consistency with tmux session behavior where notifications
// are delivered as terminal input (UI sees update, Agent receives prompt).
// The SessionID and IsBusy checks ensure we don't interrupt an active agent turn, unless urgent is true.
func (p *Propeller) notify(text string, meta map[string]string, urgent bool) error {
	if p.proxy == nil || text == "" {
		return nil
	}
	if p.proxy.SessionID() == "" {
		return fmt.Errorf("sessionID not available")
	}

	// Always notify the UI
	p.notifyWithMeta(text, meta)

	// Notify the Agent only if session is ready.
	// We bypass the IsBusy check if urgent is true (e.g. nudges/escalations).
	if p.proxy.SessionID() != "" {
		if urgent || !p.proxy.IsBusy() {
			// Try a few times in case of transient turn-state changes
			var err error
			for i := 0; i < 3; i++ {
				err = p.proxy.InjectPrompt(text)
				if err == nil {
					return nil
				}
				debugLog(p.townRoot, "[Propeller] InjectPrompt attempt %d failed: %v", i+1, err)
				time.Sleep(100 * time.Millisecond)
			}
			// Log failure to town.log
			logEvent(p.townRoot, "acp_error", fmt.Sprintf("failed to inject prompt after retries: %v", err))
			style.PrintWarning("ACP Propeller failed to inject agent prompt after retries: %v", err)
			return err
		}
	}
	// Busy and non-urgent: InjectPrompt was deliberately skipped, not
	// attempted and failed. Report this distinctly from success — only
	// notifyWithMeta's UI-only side channel ran, and the agent's active
	// turn never received the content — so the caller does not ack the
	// claim on the strength of a skip (codex, propulsion.go:267,
	// changes-requested at REVISION 3 — High 6).
	return errBusyDeferred
}
