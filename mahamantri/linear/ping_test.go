package linear

import (
	"path/filepath"
	"strings"
	"testing"
)

// IDs and payload shapes are from real deliveries in mahamantri.log.
const (
	senapatiID   = "11111111-2222-4333-8444-555555555555"
	personID      = "66666666-7777-4888-9999-000000000000"
	ownCommentID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

var senapati = Identity{UserID: senapatiID, Name: "Senapati", Handle: "senapati-bot"}

func comment(id, userID, userName, body, parentID string) string {
	parent := ""
	if parentID != "" {
		parent = `"parentId":"` + parentID + `",`
	}
	return `{"action":"create","type":"Comment","actor":{"id":"` + userID + `","name":"` + userName + `","type":"user"},` +
		`"url":"https://linear.app/acme/issue/ENG-31/x#comment-1",` +
		`"data":{"id":"` + id + `",` + parent + `"body":` + jsonString(body) + `,"issueId":"0badc0de","userId":"` + userID + `","botActor":null,` +
		`"user":{"id":"` + userID + `","name":"` + userName + `"},"issue":{"id":"0badc0de","title":"Login redirect loops","identifier":"ENG-31"}}}`
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`) + `"`
}

var (
	mentionBody    = comment("c-1", personID, "Asha", "@senapati-bot please pick this up", "")
	ownCommentBody = comment(ownCommentID, senapatiID, "Senapati", "Plan is ready - reply here to approve.", "")
)

func replyBody(parent string) string {
	return comment("c-2", personID, "Asha", "Looks good, go ahead", parent)
}

type threadSet map[string]bool

func (s threadSet) Has(id string) bool { return s[id] }

func TestClassifyMention(t *testing.T) {
	p, ok, why := Classify("Comment", []byte(mentionBody), senapati, threadSet{})
	if !ok || p.Kind != "mention" || p.Issue != "ENG-31" || p.Author != "Asha" {
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
		"issue edit":        `{"action":"update","type":"Issue","updatedFrom":{"labelIds":[]},"data":{"identifier":"ENG-31"},"actor":{"id":"` + personID + `","type":"user"}}`,
		"assignment":        `{"action":"update","type":"Issue","updatedFrom":{"assigneeId":null},"data":{"identifier":"ENG-31","assigneeId":"` + senapatiID + `"},"actor":{"id":"` + personID + `","type":"user"}}`,
		"issue created":     `{"action":"create","type":"Issue","data":{"identifier":"ENG-32"},"actor":{"id":"` + personID + `","type":"user"}}`,
		"plain comment":     comment("c-3", personID, "Asha", "note to self", ""),
		"reply elsewhere":   replyBody("other-thread"),
		"senapati's own":    comment("c-4", senapatiID, "Senapati", "@senapati-bot talking to myself", ""),
		"comment edited":    strings.Replace(mentionBody, `"action":"create"`, `"action":"update"`, 1),
		"comment removed":   strings.Replace(mentionBody, `"action":"create"`, `"action":"remove"`, 1),
		"integration":       strings.Replace(mentionBody, `"botActor":null`, `"botActor":{"type":"github"}`, 1),
		"integration actor": strings.Replace(mentionBody, `"type":"user"`, `"type":"integration"`, 1),
		"reaction":          `{"action":"create","type":"Reaction","data":{"emoji":"+1"},"actor":{"id":"` + personID + `","type":"user"}}`,
		"unparseable":       `not json`,
		"longer handle":     comment("c-5", personID, "Asha", "cc @senapati-bot2", ""),
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
		"@senapati-bot":                                     true,
		"hey @Senapati-Bot, take this":                      true,
		"thanks @senapati-bot.":                             true,
		"(@senapati-bot)":                                   true,
		"https://linear.app/acme/profiles/senapati-bot": true,
		"@Senapati can you look":                             true,
		"@senapati-bot.dev":                                 false,
		"@senapati-bot_bot":                                 false,
		"email senapati-bot@example.com":                      false,
		"senapati should do this":                            false,
	} {
		if got := senapati.mentionedIn(text); got != want {
			t.Errorf("mentionedIn(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestOwnCommentRecordsItsThread(t *testing.T) {
	if thread, issue, ok := ownComment([]byte(ownCommentBody), senapati); !ok || thread != ownCommentID || issue != "ENG-31" {
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
	if err := a.Add("t-1", "ENG-31"); err != nil {
		t.Fatal(err)
	}
	b, err := LoadThreads(path)
	if err != nil || !b.Has("t-1") || b.Has("t-2") {
		t.Errorf("reloaded: has t-1=%v t-2=%v err=%v", b.Has("t-1"), b.Has("t-2"), err)
	}
}

func TestFormatSaysItIsAPingAndHowToRouteIt(t *testing.T) {
	p, _, _ := Classify("Comment", []byte(mentionBody), senapati, nil)
	got := Format(p, nil, "http://127.0.0.1:4097")
	for _, want := range []string{
		"[Linear ping] ENG-31",
		"Asha mentioned you in a comment",
		"> @senapati-bot please pick this up",
		"Sainik for ENG-31: none yet.",
		"not a person chatting with you",
		"re-read ENG-31",
		"do not wait",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Format() missing %q:\n%s", want, got)
		}
	}
}

// Regression: left to work out the API, Senapati probed endpoints that don't
// exist and gave up. Every ping spells out the exact command for its case.
func TestFormatGivesTheExactCommandForEachCase(t *testing.T) {
	p, _, _ := Classify("Comment", []byte(mentionBody), senapati, nil)

	none := Format(p, nil, "http://127.0.0.1:4097")
	for _, want := range []string{
		`curl -s -X POST http://127.0.0.1:4097/opencode/api/session -H 'Content-Type: application/json' -d '{"title":"sainik-ENG-31-<short-name>"}'`,
		`curl -s -X POST http://127.0.0.1:4097/opencode/api/session/<id>/prompt`,
	} {
		if !strings.Contains(none, want) {
			t.Errorf("no sainik: missing %q:\n%s", want, none)
		}
	}

	busy := Format(p, []Sainik{{Label: "sainik-ENG-31-fix", SessionID: "ses_k", Status: "running", Phase: "executing"}}, "http://127.0.0.1:4097")
	for _, want := range []string{
		`"sainik-ENG-31-fix" (session ses_k): running (busy with a turn), phase "executing"`,
		`curl -s -X POST http://127.0.0.1:4097/opencode/api/session/ses_k/prompt`,
		`curl -s -X POST http://127.0.0.1:4097/opencode/api/session/ses_k/interrupt`,
		`curl -s http://127.0.0.1:4097/opencode/api/session/active | jq '.data["ses_k"]'`,
	} {
		if !strings.Contains(busy, want) {
			t.Errorf("with a sainik: missing %q:\n%s", want, busy)
		}
	}
	if strings.Contains(busy, "/opencode/api/session -H") {
		t.Errorf("with a sainik, the ping must not offer to start another:\n%s", busy)
	}
}
