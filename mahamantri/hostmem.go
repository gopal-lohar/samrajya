package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gopal-lohar/samrajya/mahamantri/bridge"
)

// availableMemory reads MemAvailable from /proc/meminfo, in bytes.
func availableMemory() (uint64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return parseMemAvailable(f)
}

func parseMemAvailable(r io.Reader) (uint64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return kb << 10, true
		}
	}
	return 0, false
}

// daemonLogTail is how much of the end of Claude Code's daemon log WhyGone
// reads.
const daemonLogTail = 512 << 10

// claudeDaemonLog is Claude Code's daemon log, where it records why it
// retired a background session.
func claudeDaemonLog() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "daemon.log")
}

// WhyGone finds the last time Claude Code's daemon says it retired the
// session ("bg retire <short id>: <reason>") and returns that reason, e.g.
// "Claude Code retired it: idle-prompt, idle 4m [low memory]".
func (l *claudeLauncher) WhyGone(s bridge.Session) string {
	if len(s.ID) < 8 {
		return ""
	}
	path := l.daemonLog
	if path == "" {
		path = claudeDaemonLog()
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > daemonLogTail {
		if _, err := f.Seek(st.Size()-daemonLogTail, io.SeekStart); err != nil {
			return ""
		}
	}
	return retireReason(f, s.ID[:8])
}

func retireReason(r io.Reader, short string) string {
	marker := "bg retire " + short + ": "
	why := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if _, after, ok := strings.Cut(sc.Text(), marker); ok {
			why = after
		}
	}
	if why == "" {
		return ""
	}
	return "Claude Code retired it: " + why
}
