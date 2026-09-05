package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeTestRepo builds a repo where main and feature have diverged on separate
// files, so a merge of main into feature succeeds without conflict.
func mergeTestRepo(t *testing.T) string {
	t.Helper()
	_, clone := setupBareAndClone(t)

	gitExec(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "feature.txt", "feature")
	gitExec(t, clone, "add", ".")
	gitExec(t, clone, "commit", "-m", "feature")

	gitExec(t, clone, "checkout", "main")
	writeFile(t, clone, "main.txt", "main update")
	gitExec(t, clone, "add", ".")
	gitExec(t, clone, "commit", "-m", "main update")
	gitExec(t, clone, "checkout", "feature")

	return clone
}

func TestIntegration_MergeRefusedBeforeStart(t *testing.T) {
	clone := mergeTestRepo(t)
	restore := withGitDir(t, clone)
	defer restore()

	// An uncommitted change to a file the merge would touch makes git refuse
	// the merge before any MERGE_HEAD is written.
	writeFile(t, clone, "main.txt", "uncommitted")

	err := Merge("main")
	require.Error(t, err)
	assert.True(t, IsMergeStartError(err))
	assert.False(t, IsMergeInProgress())
}

func TestIntegration_MergeConflictIsNotStartError(t *testing.T) {
	_, clone := setupBareAndClone(t)
	restore := withGitDir(t, clone)
	defer restore()

	gitExec(t, clone, "checkout", "-b", "feature")
	writeFile(t, clone, "shared.txt", "feature version")
	gitExec(t, clone, "add", "shared.txt")
	gitExec(t, clone, "commit", "-m", "feature edit")

	gitExec(t, clone, "checkout", "main")
	writeFile(t, clone, "shared.txt", "main version")
	gitExec(t, clone, "add", "shared.txt")
	gitExec(t, clone, "commit", "-m", "main edit")
	gitExec(t, clone, "checkout", "feature")

	err := Merge("main")
	require.Error(t, err)
	assert.False(t, IsMergeStartError(err))
	assert.True(t, IsMergeInProgress())
	gitExec(t, clone, "merge", "--abort")
}

func TestIntegration_MergeSucceedsCreatesMergeCommit(t *testing.T) {
	clone := mergeTestRepo(t)
	restore := withGitDir(t, clone)
	defer restore()

	require.NoError(t, Merge("main"))
	assert.False(t, IsMergeInProgress())

	// main is now an ancestor of feature (a merge commit records it as a parent).
	isAnc, err := IsAncestor("main", "feature")
	require.NoError(t, err)
	assert.True(t, isAnc, "main must be an ancestor of feature after merge")
}
