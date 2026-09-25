package linear

import (
	"strings"
	"testing"
)

func TestSummarizeIssueCreate(t *testing.T) {
	body := []byte(`{"action":"create","type":"Issue","data":{"identifier":"ENG-123","title":"Fix the thing"},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, Identity{})
	want := `Asha created issue ENG-123: "Fix the thing"`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestSummarizeIssueUpdateStateChange(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"updatedAt":"x","sortOrder":-1,"stateId":"old"},"data":{"identifier":"ENG-30","title":"Dark Mode","state":{"name":"Todo"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, Identity{})
	want := `Asha updated issue ENG-30 ("Dark Mode"): state changed to "Todo"`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

// Regression: an assignee change on an issue already in Todo used to be
// reported as "state is now Todo" because the summary ignored updatedFrom.
func TestSummarizeIssueUpdateAssigneeChange(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"updatedAt":"x","subscriberIds":["u"],"assigneeId":null},"data":{"identifier":"ENG-30","title":"Dark Mode","state":{"name":"Todo"},"assignee":{"name":"Senapati"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, Identity{})
	want := `Asha updated issue ENG-30 ("Dark Mode"): assigned to Senapati`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestSummarizeIssueUpdateUnassigned(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"assigneeId":"someone"},"data":{"identifier":"ENG-30","title":"T","assignee":null},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, Identity{})
	want := `Asha updated issue ENG-30 ("T"): unassigned`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestSummarizeIssueUpdateMultipleAndUnknownFields(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"updatedAt":"x","stateId":"o","assigneeId":null,"dueDate":null},"data":{"identifier":"ENG-30","title":"T","state":{"name":"In Progress"},"assignee":{"name":"Senapati"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, Identity{})
	want := `Asha updated issue ENG-30 ("T"): assigned to Senapati; dueDate changed; state changed to "In Progress"`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestSummarizeIssueUpdateOnlyNoiseFields(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"updatedAt":"x","sortOrder":1},"data":{"identifier":"ENG-30","title":"T"},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, Identity{})
	want := `Asha updated issue ENG-30: "T"`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestSummarizeCommentCreate(t *testing.T) {
	body := []byte(`{"action":"create","type":"Comment","data":{"body":"looks good","issue":{"identifier":"ENG-123"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Comment", body, Identity{})
	want := `Asha commented on issue ENG-123: "looks good"`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestSummarizeUnrecognizedShapeFallsBack(t *testing.T) {
	body := []byte(`{"action":"create","type":"SomethingNew","data":{}}`)
	got := Summarize("SomethingNew", body, Identity{})
	if got == "" {
		t.Error("Summarize() returned empty string for an unrecognized shape")
	}
}

func TestSummarizeUnparseableBodyFallsBack(t *testing.T) {
	got := Summarize("Issue", []byte("not json"), Identity{})
	if got == "" {
		t.Error("Summarize() returned empty string for unparseable JSON")
	}
}

func TestSummarizeCommentWithoutIssueIdentifier(t *testing.T) {
	body := []byte(`{"action":"create","type":"Comment","data":{"body":"on it","issueId":"abc","issue":{"id":"abc","title":"Dark Mode"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Comment", body, Identity{})
	want := `Asha commented on issue Dark Mode: "on it"`
	if got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

var senapati = Identity{UserID: "bot-id", Name: "Senapati", Handle: "senapati-bot"}

// The two assignment payloads below are shaped exactly like the real ones
// from the user's log (updatedFrom.assigneeId holds the PREVIOUS assignee).
func TestSummarizeAssignedToSenapatiIsAnInstructionToStartWork(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"updatedAt":"x","subscriberIds":["u"],"assigneeId":null},"data":{"identifier":"ENG-30","title":"Dark Mode","assigneeId":"bot-id","assignee":{"id":"bot-id","name":"Senapati"},"state":{"name":"Todo"}},"actor":{"id":"asha","name":"Asha"}}`)
	got := Summarize("Issue", body, senapati)
	for _, want := range []string{"Asha assigned issue ENG-30", "to you (Senapati)", "Take it on now"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summarize() = %q, missing %q", got, want)
		}
	}
}

func TestSummarizeAssignedAwayFromSenapati(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"assigneeId":"bot-id"},"data":{"identifier":"ENG-30","title":"Dark Mode","assigneeId":"asha","assignee":{"id":"asha","name":"Asha"}},"actor":{"id":"asha","name":"Asha"}}`)
	got := Summarize("Issue", body, senapati)
	if !strings.Contains(got, "away from you") || !strings.Contains(got, "assigned to Asha") || !strings.Contains(got, "Stop") {
		t.Errorf("Summarize() = %q", got)
	}
}

func TestSummarizeUpdateOnAnIssueAssignedToSenapati(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"stateId":"old"},"data":{"identifier":"ENG-30","title":"T","assigneeId":"bot-id","assignee":{"name":"Senapati"},"state":{"name":"In Review"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, senapati)
	if !strings.Contains(got, "which is assigned to you") || !strings.Contains(got, `state changed to "In Review"`) {
		t.Errorf("Summarize() = %q", got)
	}
}

func TestSummarizeIssueCreatedAndAssignedToSenapati(t *testing.T) {
	body := []byte(`{"action":"create","type":"Issue","data":{"identifier":"ENG-31","title":"T","assigneeId":"bot-id","assignee":{"name":"Senapati"}},"actor":{"name":"Asha"}}`)
	if got := Summarize("Issue", body, senapati); !strings.Contains(got, "assigned it to you (Senapati)") {
		t.Errorf("Summarize() = %q", got)
	}
}

func TestSummarizeUnrelatedIssueIsNeutralEvenWhenIdentityKnown(t *testing.T) {
	body := []byte(`{"action":"update","type":"Issue","updatedFrom":{"assigneeId":null},"data":{"identifier":"ENG-9","title":"T","assigneeId":"asha","assignee":{"name":"Asha"}},"actor":{"name":"Asha"}}`)
	got := Summarize("Issue", body, senapati)
	if strings.Contains(got, "you") || !strings.Contains(got, "assigned to Asha") {
		t.Errorf("an issue that isn't Senapati's must not be worded as if it were: %q", got)
	}
}

// Regression: the real comment said "@senapati-bot you are supposed to do
// this." and Senapati took that for someone else, because nothing told it
// that handle is itself.
func TestSummarizeCommentMentioningSenapatiByHandleOrName(t *testing.T) {
	for _, mention := range []string{"@senapati-bot you are supposed to do this.", "hey @Senapati, please look"} {
		body := []byte(`{"action":"create","type":"Comment","data":{"body":"` + mention + `","issue":{"id":"i","identifier":"ENG-30"}},"actor":{"name":"Asha"}}`)
		got := Summarize("Comment", body, senapati)
		if !strings.Contains(got, "mentioned you") || !strings.Contains(got, "respond to it or act on it") {
			t.Errorf("%q -> %q", mention, got)
		}
	}
	other := []byte(`{"action":"create","type":"Comment","data":{"body":"@someone else look","issue":{"identifier":"ENG-30"}},"actor":{"name":"Asha"}}`)
	if got := Summarize("Comment", other, senapati); strings.Contains(got, "mentioned you") {
		t.Errorf("a mention of someone else must not be read as Senapati: %q", got)
	}
}
