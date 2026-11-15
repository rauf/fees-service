package workflow

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
)

// TestReplay replays recorded histories against the current code. A bill runs for a month, so a
// change that would make a running bill's history replay differently fails here rather than in
// production: guard it with workflow.GetVersion. Histories come from make replay/record; add one
// for every new path a bill can take.
func TestReplay(t *testing.T) {
	histories, err := filepath.Glob("testdata/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, histories, "no recorded histories; run make replay/record")

	for _, path := range histories {
		t.Run(filepath.Base(path), func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(BillWorkflow)

			require.NoError(t, replayer.ReplayWorkflowHistoryFromJSONFile(nil, path))
		})
	}
}
