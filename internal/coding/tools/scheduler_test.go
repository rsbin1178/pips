package tools_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rsbin1178/pips/agent"
	"github.com/rsbin1178/pips/agent/catalog"
	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/internal/coding/scheduler"
	"github.com/rsbin1178/pips/internal/coding/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func schedulerStore(t *testing.T) *scheduler.Store {
	t.Helper()

	store, err := scheduler.NewStoreAt(t.TempDir(), "s-scheduler-tools", scheduler.Options{})
	require.NoError(t, err)

	return store
}

func schedulerExec(t *testing.T, tool agent.Tool, name, args string) (string, error) {
	t.Helper()

	parts, err := tool.Exec(t.Context(), agent.ToolCall{ID: "call-1", Name: name, Args: ai.JSON(args)})
	if err != nil {
		return "", err
	}

	require.Len(t, parts, 1)

	text, ok := parts[0].(ai.TextPart)
	require.True(t, ok)

	return text.Text, nil
}

func TestSchedulerCreateSchemaHidesRecurringAndAdvertisesThePatchFields(t *testing.T) {
	t.Parallel()

	tool := tools.NewSchedulerCreateTool(schedulerStore(t))
	decl := tool.Decl()
	assert.Equal(t, tools.SchedulerCreateName, decl.Name)
	assert.Contains(t, decl.Description, "fire_immediately")
	assert.Contains(t, decl.Description, "task_id")

	require.NotNil(t, decl.InputSchema)

	encoded, err := json.Marshal(decl.InputSchema)
	require.NoError(t, err)

	schema := string(encoded)
	assert.NotContains(t, schema, "recurring")

	for _, field := range []string{"task_id", "interval", "prompt", "durable", "fire_immediately"} {
		assert.Contains(t, schema, `"`+field+`"`)
	}
}

func TestSchedulerCreateAnswersIDHumanScheduleAndUpdated(t *testing.T) {
	t.Parallel()

	tool := tools.NewSchedulerCreateTool(schedulerStore(t))

	body, err := schedulerExec(t, tool, tools.SchedulerCreateName, `{"interval":"5m","prompt":"check deploy"}`)
	require.NoError(t, err)

	var created struct {
		ID            string `json:"id"`
		HumanSchedule string `json:"humanSchedule"`
		Updated       bool   `json:"updated"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "every 5 minutes", created.HumanSchedule)
	assert.False(t, created.Updated)

	body, err = schedulerExec(t, tool, tools.SchedulerCreateName,
		`{"task_id":"`+created.ID+`","interval":"10m"}`)
	require.NoError(t, err)

	var updated struct {
		ID            string `json:"id"`
		HumanSchedule string `json:"humanSchedule"`
		Updated       bool   `json:"updated"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &updated))
	assert.Equal(t, created.ID, updated.ID)
	assert.Equal(t, "every 10 minutes", updated.HumanSchedule)
	assert.True(t, updated.Updated)
}

func TestSchedulerCreateRequiresIntervalAndPromptByName(t *testing.T) {
	t.Parallel()

	tool := tools.NewSchedulerCreateTool(schedulerStore(t))

	_, err := schedulerExec(t, tool, tools.SchedulerCreateName, `{}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "interval is required")

	_, err = schedulerExec(t, tool, tools.SchedulerCreateName, `{"interval":"5m"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")

	_, err = schedulerExec(t, tool, tools.SchedulerCreateName, `{"interval":"5x","prompt":"x"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid interval suffix")
}

func TestSchedulerCreateUpdateRefusesAnUnknownIDAndAnEmptyPatch(t *testing.T) {
	t.Parallel()

	tool := tools.NewSchedulerCreateTool(schedulerStore(t))

	_, err := schedulerExec(t, tool, tools.SchedulerCreateName, `{"task_id":"missing","prompt":"x"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no scheduled task with that id")

	_, err = schedulerExec(t, tool, tools.SchedulerCreateName, `{"task_id":"missing"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to update")
}

func TestSchedulerDeleteAnswersSuccessFalseForAnUnknownID(t *testing.T) {
	t.Parallel()

	store := schedulerStore(t)
	create := tools.NewSchedulerCreateTool(store)
	deleteTool := tools.NewSchedulerDeleteTool(store)

	body, err := schedulerExec(t, create, tools.SchedulerCreateName, `{"interval":"5m","prompt":"x"}`)
	require.NoError(t, err)

	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created))

	body, err = schedulerExec(t, deleteTool, tools.SchedulerDeleteName, `{"id":"missing"}`)
	require.NoError(t, err, "an unknown id is an answer, not an error")

	var unknown struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &unknown))
	assert.False(t, unknown.Success)
	assert.Contains(t, unknown.Message, "missing")

	body, err = schedulerExec(t, deleteTool, tools.SchedulerDeleteName, `{"id":"`+created.ID+`"}`)
	require.NoError(t, err)

	var removed struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &removed))
	assert.True(t, removed.Success)
}

func TestSchedulerListAnswersTheSummaryFieldsInCreationOrder(t *testing.T) {
	t.Parallel()

	store := schedulerStore(t)
	create := tools.NewSchedulerCreateTool(store)

	first, err := schedulerExec(t, create, tools.SchedulerCreateName, `{"interval":"5m","prompt":"first"}`)
	require.NoError(t, err)

	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(first), &created))

	_, err = schedulerExec(t, create, tools.SchedulerCreateName, `{"interval":"1h","prompt":"second"}`)
	require.NoError(t, err)

	body, err := schedulerExec(t, tools.NewSchedulerListTool(store), tools.SchedulerListName, `{}`)
	require.NoError(t, err)

	var listed struct {
		Tasks []struct {
			ID            string `json:"id"`
			Prompt        string `json:"prompt"`
			IntervalHuman string `json:"intervalHuman"`
			NextFireAt    string `json:"nextFireAt"`
			CreatedAt     string `json:"createdAt"`
			Recurring     bool   `json:"recurring"`
		} `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	require.Len(t, listed.Tasks, 2)
	assert.Equal(t, created.ID, listed.Tasks[0].ID)
	assert.Equal(t, "first", listed.Tasks[0].Prompt)
	assert.Equal(t, "every 5 minutes", listed.Tasks[0].IntervalHuman)
	assert.Equal(t, "every 1 hour", listed.Tasks[1].IntervalHuman)
	assert.True(t, listed.Tasks[0].Recurring)

	_, err = time.Parse(time.RFC3339, listed.Tasks[0].NextFireAt)
	require.NoError(t, err)

	_, err = time.Parse(time.RFC3339, listed.Tasks[0].CreatedAt)
	require.NoError(t, err)
}

func TestSchedulerListTruncatesALongPrompt(t *testing.T) {
	t.Parallel()

	store := schedulerStore(t)
	create := tools.NewSchedulerCreateTool(store)

	prompt := strings.Repeat("0123456789", 20)

	_, err := schedulerExec(t, create, tools.SchedulerCreateName, `{"interval":"5m","prompt":"`+prompt+`"}`)
	require.NoError(t, err)

	body, err := schedulerExec(t, tools.NewSchedulerListTool(store), tools.SchedulerListName, `{}`)
	require.NoError(t, err)

	var listed struct {
		Tasks []struct {
			Prompt string `json:"prompt"`
		} `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &listed))
	require.Len(t, listed.Tasks, 1)
	assert.Len(t, listed.Tasks[0].Prompt, 83, "80 characters plus an ellipsis")
	assert.Less(t, len(listed.Tasks[0].Prompt), len(prompt))
}

func TestSchedulerCatalogWithoutAStoreOffersNothing(t *testing.T) {
	t.Parallel()

	empty, err := tools.NewSchedulerCatalog(nil)
	require.NoError(t, err)

	snapshot, err := empty.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskPrivileged))
	require.NoError(t, err)
	assert.Empty(t, snapshot)

	populated, err := tools.NewSchedulerCatalog(schedulerStore(t))
	require.NoError(t, err)

	snapshot, err = populated.Snapshot(t.Context(), catalog.AllowAll("test", catalog.RiskPrivileged))
	require.NoError(t, err)
	require.Len(t, snapshot, 3)

	names := make([]string, 0, len(snapshot))
	for _, tool := range snapshot {
		names = append(names, tool.Decl().Name)
	}

	assert.ElementsMatch(t, []string{
		tools.SchedulerCreateName, tools.SchedulerDeleteName, tools.SchedulerListName,
	}, names)
}
