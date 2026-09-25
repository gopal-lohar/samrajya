package attention

import "github.com/gopal-lohar/samrajya/mahamantri/opencode"

// completionTypes are the event types that mean a turn has ended - live
// testing against a real v2.0.11 server showed session.idle never actually
// fires for an ordinary single-turn prompt; session.execution.succeeded is
// the event that reliably does. session.idle is kept alongside it in case
// it fires in other circumstances (e.g. a multi-turn gap) - it's the name
// the strings-based research expected, even though it wasn't observed live.
var completionTypes = map[string]bool{
	"session.idle":                  true,
	"session.execution.succeeded":   true,
	"session.execution.failed":      true,
	"session.execution.interrupted": true,
}

// Required reports whether ev is something a manager needs to act on:
// something that blocks the agent, or a turn ending (which is when
// whatever it produced becomes reviewable - there's no independent "data
// ready for review" signal, a completed turn already is that signal).
func Required(ev opencode.Event) bool {
	return ev.Severity == opencode.Blocking || completionTypes[ev.Type]
}
