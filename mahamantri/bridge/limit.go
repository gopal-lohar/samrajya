package bridge

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The main session's usage limit. Claude Code's StopFailure hook (in the
// plugin) reports the limit message when a turn ends on it; mahamantri says
// so on Linear and, once the limit resets, wakes the session with a notice to
// carry on. Nothing else would: a turn the limit cut off does not resume, and
// pings that arrived meanwhile were already delivered into failed turns.

// A limit whose reset time cannot be read is assumed to last this long; if it
// has not, the next failed turn reports it again.
const unknownLimit = 30 * time.Minute

const limitKey = "limit.until"

var resetsRE = regexp.MustCompile(`(?i)resets\s+(?:([a-z]{3})[a-z]*\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(([^)]+)\)`)

var months = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}

// limitResets reads when a limit message such as "You've hit your 5-hour
// limit · resets 7:10pm (UTC)" or "... resets Nov 4, 9am (Asia/Kolkata)" says
// the limit resets, as the first such time after now.
func limitResets(msg string, now time.Time) (time.Time, bool) {
	m := resetsRE.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(m[6])
	if err != nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	if hour < 1 || hour > 12 || minute > 59 {
		return time.Time{}, false
	}
	hour %= 12
	if strings.EqualFold(m[5], "pm") {
		hour += 12
	}
	local := now.In(loc)
	if m[1] != "" {
		month, ok := months[strings.ToLower(m[1])]
		day, _ := strconv.Atoi(m[2])
		if !ok || day < 1 || day > 31 {
			return time.Time{}, false
		}
		t := time.Date(local.Year(), month, day, hour, minute, 0, 0, loc)
		if t.Before(now.Add(-24 * time.Hour)) { // across new year
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	}
	t := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

// limitedUntil is when the session's usage limit resets; zero if it is not
// limited.
func (s *Server) limitedUntil() time.Time {
	t, _ := time.Parse(time.RFC3339, s.Store.Meta(limitKey))
	return t
}

func (s *Server) setLimited(until time.Time) {
	v := ""
	if until.IsZero() && !s.limitedUntil().IsZero() {
		s.Store.SetMeta(limitClearedKey, s.now().Format(time.RFC3339))
	}
	if !until.IsZero() {
		v = until.Format(time.RFC3339)
		s.Store.SetMeta(limitSetKey, s.now().Format(time.RFC3339))
	}
	if err := s.Store.SetMeta(limitKey, v); err != nil {
		s.Logger.Printf("bridge: could not record the usage limit: %v", err)
	}
}

// limit records a usage limit the plugin's StopFailure hook reported.
func (s *Server) limit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
		Details string `json:"details"`
		Agent   string `json:"agent"`
		Secret  string `json:"secret"`
	}
	if !decode(w, r, &body) {
		return
	}
	agent, known := s.hookAgent(body.Agent, body.Secret)
	if !known {
		writeErr(w, http.StatusForbidden, "unknown agent or stale secret")
		return
	}
	until, ok := limitResets(body.Message+" "+body.Details, s.now())
	if !ok {
		until = s.now().Add(unknownLimit)
	}
	s.setLimited(until)
	s.Logger.Printf("bridge: %s hit the usage limit; limited until %s (%q)", agent, until.Format(time.RFC3339), truncate(body.Message, 120))
	writeJSON(w, http.StatusOK, map[string]any{"until": until})
}

// untilText is a reset time as the person would say it, in the zone the
// limit message used.
func untilText(t time.Time) string {
	return strings.Replace(t.Format("3:04pm MST"), ":00", "", 1)
}

// wakeAfterLimit clears a limit that has reset and leaves the session a
// notice to carry on, which the channel delivers at once.
func (s *Server) wakeAfterLimit() {
	until := s.limitedUntil()
	if until.IsZero() || s.now().Before(until.Add(time.Minute)) {
		return
	}
	s.setLimited(time.Time{})
	err := s.Notice("resume-"+strconv.FormatInt(until.Unix(), 10), "limit_reset",
		"Your usage limit has reset. Finish or resume the work it interrupted, and answer the pings that arrived while you were limited: samrajya_status lists the ones still pending, and the earlier messages in this conversation that failed on the limit are the rest.")
	if err != nil {
		s.Logger.Printf("bridge: could not leave the limit-reset notice: %v", err)
		return
	}
	s.Logger.Printf("bridge: usage limit reset; woke the main session")
}

// Notice stores a notice for the main session and wakes the feed.
func (s *Server) Notice(id, kind, body string) error { return s.NoticeFor(MainAgent, id, kind, body) }

// NoticeFor stores a notice for an agent and wakes the feeds.
func (s *Server) NoticeFor(agent, id, kind, body string) error {
	if err := s.Store.NoticeFor(agent, id, kind, body); err != nil {
		return err
	}
	s.Wake()
	return nil
}
