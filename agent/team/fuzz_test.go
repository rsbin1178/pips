package team

import (
	"encoding/json"
	"testing"
)

func FuzzValidateRecordNeverPanics(fuzz *testing.F) {
	fuzz.Add([]byte(`{}`))
	fuzz.Add([]byte(`{"schema_version":1}`))
	fuzz.Add([]byte(`not-json`))

	fuzz.Fuzz(func(_ *testing.T, data []byte) {
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return
		}

		_ = validateRecord(record)
	})
}

func FuzzCommittedTeamLinesNeverPanics(fuzz *testing.F) {
	fuzz.Add([]byte{})
	fuzz.Add([]byte("{}\n{}\n"))
	fuzz.Add([]byte("{}\n{\"partial\""))

	fuzz.Fuzz(func(_ *testing.T, data []byte) {
		_, _, _ = committedTeamLines("fuzz.jsonl", data)
	})
}
