package cli

import (
	"testing"

	"davit/api"
)

func TestHistoryOutput(t *testing.T) {
	for _, c := range []struct {
		e    api.HistoryEntry
		want string
	}{
		{api.HistoryEntry{StdoutBytes: 9, StderrBytes: 9}, bytesHuman(18)},
		{api.HistoryEntry{StdoutBytes: 9, OutputDropped: true}, bytesHuman(9) + " (oldest dropped)"},
		{api.HistoryEntry{OutputNotRetained: api.OutputNotRetainedPolicy}, "not retained (policy)"},
		{api.HistoryEntry{StdoutBytes: 4, OutputNotRetained: api.OutputNotRetainedPolicy},
			bytesHuman(4) + " (partly not retained: policy)"},
	} {
		if got := historyOutput(c.e); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}
