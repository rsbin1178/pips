package tui

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/rsbin1178/pips/internal/coding/question"
)

// newQuestionDetailView builds the Ctrl+T detail page for one ask_user card.
// The transcript card stays compact; this page is where the full question,
// every option and the chosen answer remain readable.
func newQuestionDetailView(block timelineBlock) toolDetailView {
	view := toolDetailView{title: "Question details"}
	if len(block.tools) == 0 {
		return view
	}

	activity := block.tools[0]
	for _, tool := range block.tools {
		if tool.id != "" {
			view.callIDs = append(view.callIDs, tool.id)
		}
	}

	rows := questionDetailHeaderRows(block)
	spec, specOK := questionDetailSpec(activity)
	result, _ := questionDetailResultFromActivity(activity)

	if result.Chat != "" {
		rows = append(rows, detailSection("Discussion"))
		rows = append(rows, detailValueRows("", result.Chat, detailToneBody, 1)...)
	}

	switch {
	case specOK:
		rows = append(rows, questionDetailSpecRows(spec, result)...)
	case isFreeformQuestionActivity(activity):
		rows = append(rows, questionDetailTextRows(activity)...)
	default:
		rows = append(rows, questionDetailFallbackRows(block, activity)...)
	}

	view.rows = clampDetailRows(rows, maximumToolDetailBytes)

	return view
}

func questionDetailHeaderRows(block timelineBlock) []toolDetailRow {
	state := toolStateSucceeded

	switch block.status {
	case questionStatusCanceled:
		state = toolStateInterrupted
	case questionStatusFailed:
		state = toolStateFailed
	}

	rows := []toolDetailRow{
		detailHeadingRow(toolActivityGlyph(state), block.title, "", state),
	}

	if block.status == questionStatusCanceled {
		rows = append(rows, detailNoteRow("No answer was submitted."))
	}

	if block.status == questionStatusFailed {
		reason := oneLineToolText(block.body)

		if reason == "" {
			reason = "The structured question could not be opened."
		}

		rows = append(rows, detailValueRows("Error", reason, detailToneBody, 1)...)
	}

	return rows
}

func questionDetailSpecRows(
	spec question.Spec,
	result questionDetailResolution,
) []toolDetailRow {
	rows := make([]toolDetailRow, 0, len(spec.Questions)*8)
	for index, item := range spec.Questions {
		rows = append(rows, detailSection(questionDetailSection(index, len(spec.Questions))))
		rows = append(rows, detailValueRows("", item.Question, detailToneBody, 1)...)

		answer := questionDetailAnswerAt(result, index)

		if item.IsText() {
			rows = append(rows, detailSection("Free text"))
		} else {
			rows = append(rows, detailSection("Options"))
			rows = append(rows, questionDetailOptionRows(item.Options, answer)...)
		}

		rows = append(rows, questionDetailAnswerRows(item, answer)...)
	}

	return rows
}

func questionDetailOptionRows(
	options []question.Option,
	answer question.Answer,
) []toolDetailRow {
	rows := make([]toolDetailRow, 0, len(options)*2)
	for _, option := range options {
		selected := slices.Contains(answer.Selections, option.Label)

		marker, tone := "○", detailToneMuted
		if selected {
			marker, tone = "●", detailToneAdded
		}

		rows = append(rows, toolDetailRow{
			tone: tone, indent: 1,
			text: marker + " " + oneLineToolText(option.Label),
		})

		if description := oneLineToolText(option.Description); description != "" {
			rows = append(rows, toolDetailRow{
				tone: detailToneMuted, indent: 2, text: description,
			})
		}
	}

	return rows
}

func questionDetailAnswerRows(item question.Question, answer question.Answer) []toolDetailRow {
	switch {
	case answer.Custom != "":
		title := "Custom answer"
		if item.IsText() {
			title = "Answer"
		}

		return append(
			[]toolDetailRow{detailSection(title)},
			detailValueRows("", answer.Custom, detailToneAdded, 1)...,
		)
	case len(answer.Selections) > 0:
		return append(
			[]toolDetailRow{detailSection("Answer")},
			detailValueRows("", strings.Join(answer.Selections, ", "), detailToneBody, 1)...,
		)
	default:
		return nil
	}
}

func isFreeformQuestionActivity(activity toolActivity) bool {
	if activity.name == question.TextToolName {
		return true
	}

	var arguments struct {
		Question string `json:"question"`
	}

	return json.Unmarshal(activity.arguments, &arguments) == nil &&
		strings.TrimSpace(arguments.Question) != ""
}

func questionDetailTextRows(activity toolActivity) []toolDetailRow {
	var arguments struct {
		Question string `json:"question"`
	}
	if json.Unmarshal(activity.arguments, &arguments) != nil ||
		strings.TrimSpace(arguments.Question) == "" {
		return nil
	}

	return append(
		[]toolDetailRow{detailSection("Question")},
		detailValueRows("", arguments.Question, detailToneBody, 1)...,
	)
}

func questionDetailFallbackRows(block timelineBlock, activity toolActivity) []toolDetailRow {
	if block.status == questionStatusCanceled || block.status == questionStatusFailed {
		return nil
	}

	body := oneLineToolText(activity.result)
	if body == "" {
		return nil
	}

	return append(
		[]toolDetailRow{detailSection("Result")},
		detailValueRows("", body, detailToneBody, 1)...,
	)
}

type questionDetailResolution struct {
	Answers []question.Answer `json:"answers"`
	Chat    string            `json:"chat"`
}

func questionDetailSpec(activity toolActivity) (question.Spec, bool) {
	var spec question.Spec
	if json.Unmarshal(activity.arguments, &spec) != nil || question.ValidateSpec(spec) != nil {
		return question.Spec{}, false
	}

	return spec, true
}

func questionDetailResultFromActivity(activity toolActivity) (questionDetailResolution, bool) {
	var result questionDetailResolution
	if json.Unmarshal([]byte(activity.result), &result) != nil {
		return questionDetailResolution{}, false
	}

	return result, true
}

func questionDetailAnswerAt(result questionDetailResolution, index int) question.Answer {
	if index < 0 || index >= len(result.Answers) {
		return question.Answer{}
	}

	return result.Answers[index]
}

func questionDetailSection(index, count int) string {
	if count > 1 {
		return "Question " + strconv.Itoa(index+1)
	}

	return "Question"
}
