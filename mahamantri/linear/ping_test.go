package linear

import (
	"path/filepath"
	"strings"
	"testing"
)

// IDs and payload shapes are from real deliveries in mahamantri.log.
const (
	senapatiID   = "076c966a-da5b-447e-a890-4b0648465fbb"
	gopalID      = "9173ca2e-0828-44ee-8fd4-24083a09105a"
	ownCommentID = "052e3f12-4a08-4f14-8041-5007e90caab3"
)

var senapati = Identity{UserID: senapatiID, Name: "Senapati", Handle: "gingermagenta"}

func comment(id, userID, userName, body, parentID string) string {
	parent := ""
	if parentID != "" {
		parent = `"parentId":"` + parentID + `",`
	}
	return `{"action":"create","type":"Comment","actor":{"id":"` + userID + `","name":"` + userName + `","type":"user"},` +
		`"url":"https://linear.app/samrajya/issue/SEN-31/x#comment-1",` +
		`"data":{"id":"` + id + `",` + parent + `"body":` + jsonString(body) + `,"issueId":"def611e8","userId":"` + userID + `","botActor":null,` +
		`"user":{"id":"` + userID + `","name":"` + userName + `"},"issue":{"id":"def611e8","title":"Request Info replies","identifier":"SEN-31"}}}`
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`) + `"`
}

var (
	mentionBody    = comment("c-1", gopalID, "Gopal", "@gingermagenta please pick this up", "")
	ownCommentBody = comment(ownCommentID, senapatiID, "Senapati", "Plan is ready - reply here to approve.", "")
)

func replyBody(parent string) string {
	return comment("c-2", gopalID, "Gopal", "Looks good, go ahead", parent)
}

type threadSet map[string]bool

func (s threadSet) Has(id string) bool { return s[id] }

func TestClassifyMention(t *testing.T) {
	p, ok, why := Classify("Comment", []byte(mentionBody), senapati, threadSet{})
	if !ok || p.Kind != "mention" || p.Issue != "SEN-31" || p.Author != "Gopal" {
		t.Fatalf("Classify = %+v, %v (%s)", p, ok, why)
	}
}

func TestClassifyReplyInSenapatisThread(t *testing.T) {
	p, ok, why := Classify("Comment", []byte(replyBody(ownCommentID)), senapati, threadSet{ownCommentID: true})
	if !ok || p.Kind != "reply" {
		t.Fatalf("Classify = %+v, %v (%s)", p, ok, why)
	}
}

func TestClassifyDropsEverythingElse(t *testing.T) {
	cases := map[string]string{
		"issue edit":        `{"action":"update","type":"Issue","updatedFrom":{"labelIds":[]},"data":{"identifier":"SEN-31"},"actor":{"id":"` + gopalID + `","type":"user"}}`,
		"assignment":        `{"action":"update","type":"Issue","updatedFrom":{"assigneeId":null},"data":{"identifier":"SEN-31","assigneeId":"` + senapatiID + `"},"actor":{"id":"` + gopalID + `","type":"user"}}`,
		"issue created":     `{"action":"create","type":"Issue","data":{"identifier":"SEN-32"},"actor":{"id":"` + gopalID + `","type":"user"}}`,
		"plain comment":     comment("c-3", gopalID, "Gopal", "note to self", ""),
		"reply elsewhere":   replyBody("other-thread"),
		"senapati's own":    comment("c-4", senapatiID, "Senapati", "@gingermagenta talking to myself", ""),
		"comment edited":    strings.Replace(mentionBody, `"action":"create"`, `"action":"update"`, 1),
		"comment removed":   strings.Replace(mentionBody, `"action":"create"`, `"action":"remove"`, 1),
		"integration":       strings.Replace(mentionBody, `"botActor":null`, `"botActor":{"type":"github"}`, 1),
		"integration actor": strings.Replace(mentionBody, `"type":"user"`, `"type":"integration"`, 1),
		"reaction":          `{"action":"create","type":"Reaction","data":{"emoji":"+1"},"actor":{"id":"` + gopalID + `","type":"user"}}`,
		"unparseable":       `not json`,
		"longer handle":     comment("c-5", gopalID, "Gopal", "cc @gingermagenta2", ""),
	}
	for name, body := range cases {
		threads := threadSet{ownCommentID: true}
		if p, ok, why := Classify("x", []byte(body), senapati, threads); ok || why == "" {
			t.Errorf("%s: Classify = %+v, %v, want dropped with a reason", name, p, ok)
		}
	}
}

func TestMentionForms(t *testing.T) {
	for text, want := range map[string]bool{
		"@gingermagenta":                                     true,
		"hey @GingerMagenta, take this":                      true,
		"thanks @gingermagenta.":                             true,
		"(@gingermagenta)":                                   true,
		"https://linear.app/samrajya/profiles/gingermagenta": true,
		"@Senapati can you look":                             true,
		"@gingermagenta.dev":                                 false,
		"@gingermagenta_bot":                                 false,
		"email gingermagenta@gmail.com":                      false,
		"senapati should do this":                            false,
	} {
		if got := senapati.mentionedIn(text); got != want {
			t.Errorf("mentionedIn(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestOwnCommentRecordsItsThread(t *testing.T) {
	if thread, issue, ok := ownComment([]byte(ownCommentBody), senapati); !ok || thread != ownCommentID || issue != "SEN-31" {
		t.Errorf("top-level: %q %q %v", thread, issue, ok)
	}
	// Senapati replying in a person's thread joins that thread.
	reply := comment("c-9", senapatiID, "Senapati", "On it", "their-thread")
	if thread, _, ok := ownComment([]byte(reply), senapati); !ok || thread != "their-thread" {
		t.Errorf("reply: %q %v", thread, ok)
	}
	if _, _, ok := ownComment([]byte(mentionBody), senapati); ok {
		t.Error("a person's comment is not Senapati's")
	}
}

func TestThreadsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	a, err := LoadThreads(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Add("t-1", "SEN-31"); err != nil {
		t.Fatal(err)
	}
	b, err := LoadThreads(path)
	if err != nil || !b.Has("t-1") || b.Has("t-2") {
		t.Errorf("reloaded: has t-1=%v t-2=%v err=%v", b.Has("t-1"), b.Has("t-2"), err)
	}
}

func TestFormatSaysItIsAPingAndHowToRouteIt(t *testing.T) {
	p, _, _ := Classify("Comment", []byte(mentionBody), senapati, nil)
	got := Format(p, "")
	for _, want := range []string{
		"[Linear ping] SEN-31",
		"Gopal mentioned you in a comment",
		"> @gingermagenta please pick this up",
		"Sainik for SEN-31: none yet.",
		"not a person chatting with you",
		"re-read SEN-31",
		"do not wait",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Format() missing %q:\n%s", want, got)
		}
	}
}
