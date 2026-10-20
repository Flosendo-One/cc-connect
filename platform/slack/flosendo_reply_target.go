package slack

// ReplyTargetKey names the thread a reply context points at as a session key ReconstructReplyCtx reads back
// ("slack:<channel>:t:<thread ts>"), so a message queued before a restart is answered in its own thread
// (EDEN-1618, core.ReplyTargetKeyer). A top-level message starts its own thread, so its ts is the thread.
func (p *Platform) ReplyTargetKey(replyCtx any) (string, bool) {
	rc, ok := replyCtx.(replyContext)
	if !ok || rc.channel == "" {
		return "", false
	}
	ts := rc.timestamp
	if ts == "" {
		ts = rc.messageTS
	}
	if ts == "" {
		return "slack:" + rc.channel, true
	}
	return "slack:" + rc.channel + ":t:" + ts, true
}
