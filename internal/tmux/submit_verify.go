package tmux

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrSubmitNotVerified reports that a nudge payload was typed, but the
// transport could not prove it left the target composer.
var ErrSubmitNotVerified = errors.New("submit not verified: message stranded in composer")

// ErrComposerDirty reports a composer state where retyping the message on
// the next attempt would corrupt or duplicate content rather than fix
// anything: either the composer holds normal (non-ghost) text that is
// neither the sent needle nor its prefix, or the needle itself is still
// sitting there stranded and no validated recovery keystroke can clear it
// first. Either way, callers must not retry delivery on this error — see
// nudge.MaxInjectionAttempts and the poller's dead-letter path. Always
// wrapped together with ErrSubmitNotVerified; check with errors.Is.
var ErrComposerDirty = errors.New("composer dirty: retyping would corrupt or duplicate content")

type submitProbe int

const (
	probeUnknown submitProbe = iota
	probeTurnStarted
	probeComposerCleared
	probeStranded
	probeComposerDirty
)

func (p submitProbe) String() string {
	switch p {
	case probeTurnStarted:
		return "turn-started"
	case probeComposerCleared:
		return "composer-cleared"
	case probeStranded:
		return "stranded"
	case probeComposerDirty:
		return "composer-dirty"
	default:
		return "unknown"
	}
}

const (
	submitProbeAttempts  = 3
	submitProbeInterval  = 700 * time.Millisecond
	submitNeedleMaxRunes = 32
	minStrandPrefixRunes = 8
)

func submitNeedle(message string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > submitNeedleMaxRunes {
			return string(runes[:submitNeedleMaxRunes])
		}
		return line
	}
	return ""
}

func applySGR(params string, dim bool) bool {
	if params == "" {
		return false
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "", "0":
			dim = false
		case "2":
			dim = true
		case "22":
			dim = false
		case "38", "48", "58":
			if i+1 >= len(fields) {
				continue
			}
			switch fields[i+1] {
			case "5":
				i += 2
			case "2":
				i += 4
			}
		}
	}
	return dim
}

func stripAnsiTrackDim(s string) ([]rune, []bool) {
	var plain []rune
	var dim []bool
	curDim := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j >= len(s) {
					break
				}
				if s[j] == 'm' {
					curDim = applySGR(s[i+2:j], curDim)
				}
				i = j + 1
				continue
			}
			if i+1 < len(s) && s[i+1] == ']' {
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j >= len(s) {
					break
				}
				if s[j] == 0x1b {
					j++
				}
				i = j + 1
				continue
			}
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		plain = append(plain, r)
		dim = append(dim, curDim)
		i += size
	}
	return plain, dim
}

func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func allDim(flags []bool) bool {
	if len(flags) == 0 {
		return false
	}
	for _, flag := range flags {
		if !flag {
			return false
		}
	}
	return true
}

func splitRunesAndDim(plain []rune, dim []bool) ([][]rune, [][]bool) {
	var lines [][]rune
	var lineDims [][]bool
	start := 0
	for i := 0; i <= len(plain); i++ {
		if i == len(plain) || plain[i] == '\n' {
			lines = append(lines, plain[start:i])
			lineDims = append(lineDims, dim[start:i])
			start = i + 1
		}
	}
	return lines, lineDims
}

func trimRunesAndDim(runes []rune, dim []bool) ([]rune, []bool) {
	isSpace := func(r rune) bool { return r == ' ' || r == '\t' || r == '\u00a0' }
	for len(runes) > 0 && isSpace(runes[0]) {
		runes = runes[1:]
		dim = dim[1:]
	}
	for len(runes) > 0 && isSpace(runes[len(runes)-1]) {
		runes = runes[:len(runes)-1]
		dim = dim[:len(dim)-1]
	}
	return runes, dim
}

func composerContent(line []rune, dim []bool, promptPrefix string) ([]rune, []bool) {
	prefix := []rune(strings.TrimSpace(strings.ReplaceAll(promptPrefix, "\u00a0", " ")))
	if len(prefix) == 0 {
		return nil, nil
	}
	idx := runeIndex(line, prefix)
	if idx < 0 {
		idx = runeIndex(line, prefix[:1])
	}
	if idx < 0 {
		return nil, nil
	}
	after := line[idx+len(prefix):]
	afterDim := dim[idx+len(prefix):]
	return trimRunesAndDim(after, afterDim)
}

func analyzeComposerLine(line []rune, dim []bool, needle, promptPrefix string) submitProbe {
	needleRunes := []rune(needle)
	if idx := runeIndex(line, needleRunes); idx >= 0 {
		if allDim(dim[idx : idx+len(needleRunes)]) {
			return probeComposerCleared
		}
		return probeStranded
	}

	content, contentDim := composerContent(line, dim, promptPrefix)
	if len(content) == 0 {
		return probeComposerCleared
	}
	if allDim(contentDim) {
		return probeComposerCleared
	}
	if len(content) >= minStrandPrefixRunes && strings.HasPrefix(needle, string(content)) {
		return probeStranded
	}
	return probeComposerDirty
}

func analyzeSubmission(escContent, needle, promptPrefix string) submitProbe {
	if needle == "" || promptPrefix == "" {
		return probeUnknown
	}
	plain, dim := stripAnsiTrackDim(escContent)
	lines, lineDims := splitRunesAndDim(plain, dim)

	for i := len(lines) - 1; i >= 0; i-- {
		if matchesPromptPrefix(string(lines[i]), promptPrefix) {
			return analyzeComposerLine(lines[i], lineDims[i], needle, promptPrefix)
		}
	}

	for _, line := range lines {
		if hasBusyIndicator(string(line)) {
			return probeTurnStarted
		}
	}
	return probeUnknown
}

func (t *Tmux) probeSubmission(target, needle, promptPrefix string) submitProbe {
	content, err := t.run("capture-pane", "-p", "-e", "-t", target, "-S", "-25")
	if err != nil {
		return probeUnknown
	}
	return analyzeSubmission(content, needle, promptPrefix)
}

func (t *Tmux) pollSubmission(target, needle, promptPrefix string, attempts int) submitProbe {
	last := probeUnknown
	stranded := false
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(submitProbeInterval)
		}
		probe := t.probeSubmission(target, needle, promptPrefix)
		switch probe {
		case probeTurnStarted, probeComposerCleared:
			return probe
		case probeComposerDirty:
			return probe
		case probeStranded:
			stranded = true
		}
		last = probe
	}
	if stranded {
		return probeStranded
	}
	return last
}

// submitComposer verifies that Enter delivered the message, and only attempts
// stranded-composer recovery keystrokes (C-j) when recoveryValidated is true.
// Recovery keystrokes are runtime-specific and unvalidated on most non-Claude
// presets (see config.AgentPresetInfo.RecoveryKeystrokesValidated); sending
// them blind risks a destructive effect (e.g., aborting in-flight generation)
// rather than merely resetting the composer.
func (t *Tmux) submitComposer(target, message, promptPrefix string, recoveryValidated bool) error {
	enterErr := t.sendEnterVerified(target)
	needle := submitNeedle(message)
	if needle == "" {
		return enterErr
	}

	switch t.pollSubmission(target, needle, promptPrefix, submitProbeAttempts) {
	case probeTurnStarted, probeComposerCleared:
		return nil
	case probeUnknown:
		// Genuinely indeterminate: the pane content matched neither a
		// known-good nor a known-dirty pattern after every poll attempt.
		// The previous version returned enterErr bare — a nil enterErr
		// then reported SUCCESS despite never having confirmed anything,
		// and a non-nil enterErr that doesn't itself wrap
		// ErrSubmitNotVerified made errors.Is(deliverErr,
		// ErrSubmitNotVerified) false, routing callers to the bounded
		// (retypable) failure path for a case REVISION 3 requires zero
		// retypes on (codex, submit_verify.go:312, changes-requested at
		// 08964387/95f841e6 rework). Always wrap ErrSubmitNotVerified here.
		if enterErr != nil {
			return fmt.Errorf("%w: %w", ErrSubmitNotVerified, enterErr)
		}
		return fmt.Errorf("%w (composer state indeterminate after Enter)", ErrSubmitNotVerified)
	case probeComposerDirty:
		return fmt.Errorf("%w: %w (composer contains other text after Enter)", ErrSubmitNotVerified, ErrComposerDirty)
	case probeStranded:
		if !recoveryValidated {
			// Wrapped with ErrComposerDirty too, even though the pane state
			// is "stranded" not "dirty": the needle text is still sitting in
			// the composer, so a caller that treats this as an ordinary
			// bounded failure and retypes on the next attempt would type the
			// new message straight on top of the stranded leftover with no
			// clear step first — corrupting the composer exactly as a real
			// dirty-composer retype would. Callers must not retry either.
			return fmt.Errorf("%w: %w (stranded; recovery keystrokes not validated for this runtime)", ErrSubmitNotVerified, ErrComposerDirty)
		}
		return t.recoverStrandedComposer(target, message, needle, promptPrefix)
	default:
		// Unreachable with the current submitProbe enum (every value has an
		// explicit case above); kept only so the switch compiles without a
		// bare fallthrough. Wraps ErrSubmitNotVerified for the same reason
		// as probeUnknown: an unrecognized probe result must never look
		// like an ordinary retypable failure to a caller checking
		// errors.Is(deliverErr, ErrSubmitNotVerified).
		return fmt.Errorf("%w (unrecognized submit probe result)", ErrSubmitNotVerified)
	}
}

func (t *Tmux) recoverStrandedComposer(target, message, needle, promptPrefix string) error {
	if _, err := t.run("send-keys", "-t", target, "C-j"); err != nil {
		return fmt.Errorf("%w (C-j reset failed: %v)", ErrSubmitNotVerified, err)
	}
	time.Sleep(500 * time.Millisecond)

	switch probe := t.probeSubmission(target, needle, promptPrefix); probe {
	case probeTurnStarted:
		return nil
	case probeComposerCleared:
		if err := t.sendMessageToTarget(target, message); err != nil {
			return fmt.Errorf("%w (retype failed: %v)", ErrSubmitNotVerified, err)
		}
		time.Sleep(adaptiveTextDelay(len(message)))
		_ = t.sendEnterVerified(target)
	default:
		return recoveryProbeError(probe)
	}

	switch probe := t.pollSubmission(target, needle, promptPrefix, submitProbeAttempts); probe {
	case probeTurnStarted, probeComposerCleared:
		return nil
	default:
		return fmt.Errorf("nudge submit to %q: %w (final state: %s)", target, ErrSubmitNotVerified, probe)
	}
}

// recoveryProbeError builds the error for a post-C-j probe result that is
// neither turn-started nor composer-cleared: probeStranded (the needle is
// still visibly sitting in the composer) and probeComposerDirty (the
// composer holds other content) both wrap ErrComposerDirty alongside
// ErrSubmitNotVerified, so a caller keying on either error sees the same
// "do not retype" signal a fresh (non-recovery) dirty/stranded probe already
// gives; probeUnknown wraps ErrSubmitNotVerified alone, since it is
// genuinely indeterminate rather than known-dirty. Split out as a pure
// function so this classification is unit-testable without a live tmux
// session — the inline version could only be exercised end-to-end (codex,
// submit_verify.go:348 / submit_verify_test.go:158, changes-requested at
// 08964387: the existing wrapping tests constructed their own error values
// rather than calling production code, so they could not fail if this
// classification broke).
func recoveryProbeError(probe submitProbe) error {
	switch probe {
	case probeStranded, probeComposerDirty:
		return fmt.Errorf("%w: %w (composer state after C-j: %s)", ErrSubmitNotVerified, ErrComposerDirty, probe)
	default:
		return fmt.Errorf("%w (composer state after C-j: %s)", ErrSubmitNotVerified, probe)
	}
}
