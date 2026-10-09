package core

import (
	"regexp"
	"strings"
)

// Flosendo: what an idle turn (the orphan wakeup path and the engine's unsolicited relay) may post.
// The NO_REPLY marker means "say nothing": a bare marker posts nothing; a trailing marker is dropped and
// the text before it is posted. Claude Code emits each text block whole, and the idle paths join the
// blocks, so a closing "NO_REPLY" block arrived glued to the sentence before it ("...done).NO_REPLY",
// #eden-dev 9 Oct 2026) and slipped past the platform-wide trailing rule, which wants a space before it.
// Here the marker counts after any non-word character.
var flosendoTrailingSilentRe = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])NO_REPLY[\s.!]*$`)

// flosendoIdleOutbound returns the text to post and whether nothing should be posted.
func flosendoIdleOutbound(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false // no text is not a decision to be silent (a tool-only turn still shows)
	}
	if isSilentReply(t) {
		return "", true
	}
	if loc := flosendoTrailingSilentRe.FindStringSubmatchIndex(t); loc != nil {
		out := strings.TrimSpace(t[:loc[3]]) // keep the character before the marker (a full stop)
		return out, out == ""
	}
	return t, false
}

// flosendoJoinBlocks joins an idle turn's text blocks as paragraphs: Claude Code emits whole blocks,
// and joining them with "" glued sentences together ("...building it.Now the tests first").
func flosendoJoinBlocks(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}
