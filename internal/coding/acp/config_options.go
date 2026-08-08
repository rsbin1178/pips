package acp

import (
	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/rsbin/pips/internal/coding"
	"github.com/rsbin/pips/internal/coding/config"
	"github.com/rsbin/pips/internal/coding/modelcatalog"
)

const (
	sessionModeConfigID  acpsdk.SessionConfigId = "mode"
	sessionModelConfigID acpsdk.SessionConfigId = "model"
)

func sessionConfigOptions(
	state coding.State,
	models []modelcatalog.Entry,
) []acpsdk.SessionConfigOption {
	options := []acpsdk.SessionConfigOption{sessionModeConfigOption(state.Mode)}
	if model, ok := sessionModelConfigOption(state, models); ok {
		options = append(options, model)
	}

	return options
}

func sessionModeConfigOption(mode coding.OperatingMode) acpsdk.SessionConfigOption {
	description := "Controls whether Pips plans or executes coding work"
	agentDescription := "Execute coding work through the normal permission boundary"
	planDescription := "Research and design without implementation changes"
	values := acpsdk.SessionConfigSelectOptionsUngrouped{
		{
			Value:       acpsdk.SessionConfigValueId(coding.ModeAgent),
			Name:        "Agent",
			Description: &agentDescription,
		},
		{
			Value:       acpsdk.SessionConfigValueId(coding.ModePlan),
			Name:        "Plan",
			Description: &planDescription,
		},
	}

	return acpsdk.SessionConfigOption{Select: &acpsdk.SessionConfigOptionSelect{
		Id:           sessionModeConfigID,
		Name:         "Session mode",
		Description:  &description,
		Category:     new(acpsdk.SessionConfigOptionCategoryMode),
		Type:         "select",
		CurrentValue: acpsdk.SessionConfigValueId(mode),
		Options: acpsdk.SessionConfigSelectOptions{
			Ungrouped: &values,
		},
	}}
}

func sessionModelConfigOption(
	state coding.State,
	models []modelcatalog.Entry,
) (acpsdk.SessionConfigOption, bool) {
	current := config.ModelRef{Provider: state.Provider, Model: state.ModelID}.String()
	if current == "" {
		return acpsdk.SessionConfigOption{}, false
	}

	values := make(acpsdk.SessionConfigSelectOptionsUngrouped, 0, len(models))
	currentFound := false

	for _, model := range models {
		value := model.Ref.String()
		if value == "" {
			continue
		}

		if value == current {
			currentFound = true
		}

		values = append(values, acpsdk.SessionConfigSelectOption{
			Value: acpsdk.SessionConfigValueId(value),
			Name:  value,
		})
	}

	if len(values) == 0 || !currentFound {
		return acpsdk.SessionConfigOption{}, false
	}

	description := "Select the model used for this ACP session"

	return acpsdk.SessionConfigOption{Select: &acpsdk.SessionConfigOptionSelect{
		Id:           sessionModelConfigID,
		Name:         "Model",
		Description:  &description,
		Category:     new(acpsdk.SessionConfigOptionCategoryModel),
		Type:         "select",
		CurrentValue: acpsdk.SessionConfigValueId(current),
		Options: acpsdk.SessionConfigSelectOptions{
			Ungrouped: &values,
		},
	}}, true
}

func configOptionUpdate(options []acpsdk.SessionConfigOption) acpsdk.SessionUpdate {
	return acpsdk.SessionUpdate{ConfigOptionUpdate: &acpsdk.SessionConfigOptionUpdate{
		ConfigOptions: options,
	}}
}

func modeUpdates(
	mode coding.OperatingMode,
	options []acpsdk.SessionConfigOption,
) []acpsdk.SessionUpdate {
	return []acpsdk.SessionUpdate{modeUpdate(mode), configOptionUpdate(options)}
}
