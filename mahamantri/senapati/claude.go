package senapati

import (
	"context"

	"github.com/gopal-lohar/samrajya/mahamantri/claudecode"
	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

// ClaudeSender runs Senapati itself on the Claude Code harness: the Manager's
// session operations, in Claude Code's terms. Args are Senapati's lockdown
// flags (ClaudeSenapatiArgs), applied on creation and re-applied to a resumed
// session (via SetPermissions, which the Manager calls on resume).
type ClaudeSender struct {
	H             *claudecode.Harness
	Args          []string
	FallbackModel string // switched to when Senapati hits a usage limit

	models []opencode.ModelInfo // the context window last seen, for usage
}

func (c *ClaudeSender) CreateSession(_ context.Context, req opencode.CreateSessionRequest) (opencode.SessionInfo, error) {
	spec := claudecode.Spec{Title: req.Title, Agent: req.Agent, Args: c.Args, FallbackModel: c.FallbackModel}
	if req.Location != nil {
		spec.Dir = req.Location.Directory
	}
	if req.Model != nil {
		spec.Model = req.Model.ID
	}
	id, err := c.H.Create(spec)
	return opencode.SessionInfo{ID: id, Title: req.Title}, err
}

func (c *ClaudeSender) GetSession(_ context.Context, id string) (opencode.SessionInfo, error) {
	info, err := c.H.Info(id)
	return opencode.SessionInfo{ID: info.ID, Title: info.Title}, err
}

func (c *ClaudeSender) Prompt(_ context.Context, id string, req opencode.PromptRequest) (opencode.PromptResponse, error) {
	return opencode.PromptResponse{SessionID: id}, c.H.Prompt(id, req.Text, req.Delivery)
}

func (c *ClaudeSender) SwitchModel(_ context.Context, id string, m opencode.SessionModel) error {
	return c.H.SetModel(id, m.ID)
}

func (c *ClaudeSender) SwitchAgent(_ context.Context, id, agent string) error {
	return c.H.SetAgent(id, agent)
}

// SetPermissions ignores opencode rules: on Claude Code, Senapati's lockdown
// is its flags, re-applied here.
func (c *ClaudeSender) SetPermissions(_ context.Context, id string, _ []opencode.PermissionRule) error {
	return c.H.SetArgs(id, c.Args)
}

// PutInstruction installs the role as the session's appended system prompt:
// part of every request, so it survives compaction.
func (c *ClaudeSender) PutInstruction(_ context.Context, id, _ string, value string) error {
	return c.H.SetRole(id, value)
}

// ListModels and LatestAssistantMessage give the Manager what it needs to
// measure context usage for rotation, from the session's last turn.
func (c *ClaudeSender) ListModels(context.Context) ([]opencode.ModelInfo, error) {
	return c.models, nil
}

func (c *ClaudeSender) LatestAssistantMessage(_ context.Context, id string) (opencode.AssistantMessage, bool, error) {
	tokens, window, model, ok := c.H.Usage(id)
	if !ok {
		return opencode.AssistantMessage{}, false, nil
	}
	c.models = []opencode.ModelInfo{{ID: model, Limit: opencode.ModelLimit{Context: window}}}
	return opencode.AssistantMessage{ModelID: model, Tokens: opencode.TokenUsage{Input: tokens}}, true, nil
}
