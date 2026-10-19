package session_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsbin1178/pips/agent/harness"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositoryForkSubagentInheritsTranscriptWithoutBookkeeping(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	parent, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = parent.Session().AppendMessage(ai.UserText("delegate the edit"), nil)
	require.NoError(t, err)

	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
		Kind: session.KindSubagent, ParentSessionID: parent.Metadata().ID,
		Agent: "explore", SubagentIdentity: testSubagentIdentity("explore"),
	})
	require.NoError(t, err)
	_, err = source.Session().AppendMessage(ai.UserText("first task"), nil)
	require.NoError(t, err)
	_, err = source.Session().AppendMessage(ai.AssistantText("first answer"), nil)
	require.NoError(t, err)
	_, err = source.Session().AppendCustom(
		"pips.coding.subagent.terminal", ai.JSON(`{"state":"succeeded"}`),
	)
	require.NoError(t, err)

	identity := testSubagentIdentity("explore")
	identity.PlanDigest = strings.Repeat("c", 64)
	resumed, err := repo.ForkSubagent(t.Context(), source, session.ForkSubagentOptions{
		ParentSessionID: parent.Metadata().ID, Agent: "explore", SubagentIdentity: identity,
	})
	require.NoError(t, err)

	assert.NotEqual(t, source.Metadata().ID, resumed.Metadata().ID)
	assert.Equal(t, session.KindSubagent, resumed.Metadata().Kind)
	assert.Equal(t, parent.Metadata().ID, resumed.Metadata().ParentSessionID)
	assert.Equal(t, "explore", resumed.Metadata().Agent)
	assert.Equal(t, strings.Repeat("c", 64), resumed.Metadata().SubagentIdentity.PlanDigest)

	context, err := resumed.Session().Context()
	require.NoError(t, err)
	require.Len(t, context.Messages, 2)
	assert.Equal(t, "first task", subagentMessageText(context.Messages[0]))
	assert.Equal(t, "first answer", subagentMessageText(context.Messages[1]))

	// The source's own bookkeeping entries are not copied, so the resumed child
	// never claims the source's lifecycle or the changes it already reported.
	for _, entry := range resumed.Session().Entries() {
		assert.NotEqual(t, harness.KindCustom, entry.Kind)
	}

	// Both children belong to the parent, which is what keeps the durable
	// parent index consistent.
	children, err := repo.ListSubagents(t.Context(), parent.Metadata())
	require.NoError(t, err)
	assert.Len(t, children, 2)

	// The source stays unchanged, so the same child can be resumed again.
	sourceContext, err := source.Session().Context()
	require.NoError(t, err)
	require.Len(t, sourceContext.Messages, 2)
	assert.Equal(t, "first answer", subagentMessageText(sourceContext.Messages[1]))

	require.NoError(t, resumed.Close())
	require.NoError(t, source.Close())
	require.NoError(t, parent.Close())
}

func TestRepositoryForkSubagentNeedsAParentAndIdentity(t *testing.T) {
	t.Parallel()

	repo, err := session.NewRepository(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	source, err := repo.Create(t.Context(), session.CreateOptions{
		WorkspaceID: "workspace-key", WorkspacePath: testWorkspacePath,
	})
	require.NoError(t, err)
	_, err = source.Session().AppendMessage(ai.UserText("first task"), nil)
	require.NoError(t, err)

	_, err = repo.ForkSubagent(t.Context(), source, session.ForkSubagentOptions{
		ParentSessionID: "parent", Agent: "explore",
	})
	require.ErrorIs(t, err, session.ErrInvalid)

	_, err = repo.ForkSubagent(t.Context(), source, session.ForkSubagentOptions{
		ParentSessionID: "parent", Agent: "explore", SubagentIdentity: &session.SubagentIdentity{},
	})
	require.ErrorIs(t, err, session.ErrInvalid)

	_, err = repo.ForkSubagent(t.Context(), source, session.ForkSubagentOptions{
		Agent: "explore", SubagentIdentity: testSubagentIdentity("explore"),
	})
	require.ErrorIs(t, err, session.ErrInvalid)

	require.NoError(t, source.Close())
}

func subagentMessageText(message ai.Message) string {
	var value strings.Builder

	parts, err := ai.MessageParts(message)
	if err != nil {
		return ""
	}

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			value.WriteString(text.Text)
		}
	}

	return value.String()
}
