package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// API is the little of Linear's GraphQL API mahamantri uses itself, as
// Senapati (the key's user): marking pings received, setting Senapati's
// status and telling the person a ping is still waiting. The main session
// does everything else through its own Linear tools.
type API struct {
	Key string
	// URL defaults to Linear's GraphQL endpoint.
	URL    string
	Client *http.Client

	mu sync.Mutex
	me string
}

const graphqlURL = "https://api.linear.app/graphql"

func (a *API) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	url := a.URL
	if url == "" {
		url = graphqlURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.Key)
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var reply struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("linear api: %s: %.200s", res.Status, raw)
	}
	if len(reply.Errors) > 0 {
		return fmt.Errorf("linear api: %s", reply.Errors[0].Message)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("linear api: %s", res.Status)
	}
	if out != nil {
		return json.Unmarshal(reply.Data, out)
	}
	return nil
}

// React adds emoji to a comment as Senapati.
func (a *API) React(ctx context.Context, commentID, emoji string) error {
	var out struct {
		ReactionCreate struct {
			Success bool `json:"success"`
		} `json:"reactionCreate"`
	}
	err := a.do(ctx, `mutation($input: ReactionCreateInput!) { reactionCreate(input: $input) { success } }`,
		map[string]any{"input": map[string]string{"commentId": commentID, "emoji": emoji}}, &out)
	if err == nil && !out.ReactionCreate.Success {
		err = errors.New("linear api: reaction not created")
	}
	return err
}

// SetStatus sets Senapati's Linear status; an empty label clears it.
func (a *API) SetStatus(ctx context.Context, emoji, label string) error {
	me, err := a.viewer(ctx)
	if err != nil {
		return err
	}
	input := map[string]any{"statusEmoji": emoji, "statusLabel": label}
	if label == "" {
		input = map[string]any{"statusEmoji": nil, "statusLabel": nil, "statusUntilAt": nil}
	}
	var out struct {
		UserUpdate struct {
			Success bool `json:"success"`
		} `json:"userUpdate"`
	}
	err = a.do(ctx, `mutation($id: String!, $input: UserUpdateInput!) { userUpdate(id: $id, input: $input) { success } }`,
		map[string]any{"id": me, "input": input}, &out)
	if err == nil && !out.UserUpdate.Success {
		err = errors.New("linear api: status not updated")
	}
	return err
}

func (a *API) viewer(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.me != "" {
		return a.me, nil
	}
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := a.do(ctx, `query { viewer { id } }`, nil, &out); err != nil {
		return "", err
	}
	a.me = out.Viewer.ID
	return a.me, nil
}

// Comment posts body on an issue as Senapati, in the thread parentID if set.
func (a *API) Comment(ctx context.Context, issueID, parentID, body string) error {
	input := map[string]string{"issueId": issueID, "body": body}
	if parentID != "" {
		input["parentId"] = parentID
	}
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	err := a.do(ctx, `mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success } }`,
		map[string]any{"input": input}, &out)
	if err == nil && !out.CommentCreate.Success {
		err = errors.New("linear api: comment not created")
	}
	return err
}

// Handle is what a comment @-mentions a user as.
func (a *API) Handle(ctx context.Context, userID string) (string, error) {
	var out struct {
		User struct {
			DisplayName string `json:"displayName"`
		} `json:"user"`
	}
	if err := a.do(ctx, `query($id: String!) { user(id: $id) { displayName } }`, map[string]any{"id": userID}, &out); err != nil {
		return "", err
	}
	return out.User.DisplayName, nil
}
