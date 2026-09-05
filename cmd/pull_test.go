package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/stack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mergeCall records a git merge performed by the pull cascade: the base merged
// in, and the branch it was merged into (the branch checked out at the time).
type mergeCall struct {
	base   string
	branch string
}

// newPullMock creates a MockOps pre-configured for pull tests. It tracks the
// checked-out branch so merges can be attributed to a target branch, and
// returns stable SHAs based on ref name.
func newPullMock(tmpDir, currentBranch string, calls *[]mergeCall) *git.MockOps {
	current := currentBranch
	return &git.MockOps{
		GitDirFn:        func() (string, error) { return tmpDir, nil },
		CurrentBranchFn: func() (string, error) { return currentBranch, nil },
		RevParseFn: func(ref string) (string, error) {
			return "sha-" + ref, nil
		},
		IsAncestorFn:        func(string, string) (bool, error) { return true, nil },
		FetchFn:             func(string) error { return nil },
		EnableRerereFn:      func() error { return nil },
		IsMergeInProgressFn: func() bool { return false },
		CheckoutBranchFn: func(name string) error {
			current = name
			return nil
		},
		MergeFn: func(base string) error {
			*calls = append(*calls, mergeCall{base: base, branch: current})
			return nil
		},
	}
}

// TestPull_CascadeMerge verifies that a stack [b1, b2, b3] with all active
// branches merges bottom-up: trunk into b1, b1 into b2, b2 into b3.
func TestPull_CascadeMerge(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b2", &calls)

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.NoError(t, err)
	require.Len(t, calls, 3)
	assert.Equal(t, mergeCall{"main", "b1"}, calls[0], "trunk should be merged into b1")
	assert.Equal(t, mergeCall{"b1", "b2"}, calls[1], "b1 should be merged into b2")
	assert.Equal(t, mergeCall{"b2", "b3"}, calls[2], "b2 should be merged into b3")
	assert.Contains(t, output, "updated locally by merging")
}

// TestPull_MergedBranch_SkipsAndUsesEffectiveParent verifies that a merged
// branch is skipped and its descendants merge from the effective parent (trunk
// for the branch that followed the merged one). No --onto machinery is involved.
func TestPull_MergedBranch_SkipsAndUsesEffectiveParent(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1", PullRequest: &stack.PullRequestRef{Number: 10, Merged: true}},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b2", &calls)
	mock.BranchExistsFn = func(string) bool { return true }

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.NoError(t, err)
	assert.Contains(t, output, "Skipping b1")
	require.Len(t, calls, 2)
	assert.Equal(t, mergeCall{"main", "b2"}, calls[0], "b2 should merge trunk (b1 is merged)")
	assert.Equal(t, mergeCall{"b2", "b3"}, calls[1], "b3 should merge b2")
}

// TestPull_QueuedBranch_DownstreamMergesQueued verifies that a queued branch is
// skipped but downstream branches merge from it, keeping its not-yet-landed
// commits in the stack.
func TestPull_QueuedBranch_DownstreamMergesQueued(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1", PullRequest: &stack.PullRequestRef{Number: 10}},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b2", &calls)
	mock.BranchExistsFn = func(string) bool { return true }

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cfg.GitHubClientOverride = queuedPRClient(map[int]string{10: "b1"})
	cmd := PullCmd(cfg)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.NoError(t, err)
	assert.Contains(t, output, "Skipping b1")
	assert.Contains(t, output, "queued")
	require.Len(t, calls, 2)
	assert.Equal(t, mergeCall{"b1", "b2"}, calls[0], "b2 should merge the queued b1, keeping its commits")
	assert.Equal(t, mergeCall{"b2", "b3"}, calls[1], "b3 should merge b2")
}

// TestPull_DownstackOnly verifies that --downstack only merges branches from
// trunk to the current branch (inclusive).
func TestPull_DownstackOnly(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b2", &calls)

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, _ := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetArgs([]string{"--downstack"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Out.Close()
	cfg.Err.Close()

	assert.NoError(t, err)
	require.Len(t, calls, 2, "downstack should merge b1 and b2 only")
	assert.Equal(t, mergeCall{"main", "b1"}, calls[0])
	assert.Equal(t, mergeCall{"b1", "b2"}, calls[1])
}

// TestPull_UpstackOnly verifies that --upstack only merges branches from the
// current branch to the top.
func TestPull_UpstackOnly(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b2", &calls)

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, _ := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetArgs([]string{"--upstack"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Out.Close()
	cfg.Err.Close()

	assert.NoError(t, err)
	require.Len(t, calls, 2, "upstack should merge b2 and b3 only")
	assert.Equal(t, mergeCall{"b1", "b2"}, calls[0], "b2 should merge its parent b1")
	assert.Equal(t, mergeCall{"b2", "b3"}, calls[1])
}

// TestPull_NoTrunk_SkipsFirstBranchAndFetch verifies that --no-trunk skips the
// branch that would merge trunk and performs no fetch.
func TestPull_NoTrunk_SkipsFirstBranchAndFetch(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	fetched := false
	mock := newPullMock(tmpDir, "b2", &calls)
	mock.FetchFn = func(string) error { fetched = true; return nil }
	mock.FetchBranchFn = func(string, string) error { fetched = true; return nil }
	mock.FetchBranchesFn = func(string, []string) error { fetched = true; return nil }

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, _ := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetArgs([]string{"--no-trunk"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Out.Close()
	cfg.Err.Close()

	assert.NoError(t, err)
	assert.False(t, fetched, "--no-trunk must not fetch from remote")
	require.Len(t, calls, 2, "--no-trunk should skip merging trunk into b1")
	assert.Equal(t, mergeCall{"b1", "b2"}, calls[0])
	assert.Equal(t, mergeCall{"b2", "b3"}, calls[1])
}

// TestPull_ConflictSavesState verifies that a merge conflict pauses the pull,
// prints continue guidance, and persists resumable state.
func TestPull_ConflictSavesState(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	var calls []mergeCall
	current := "b1"
	mock := newPullMock(tmpDir, "b1", &calls)
	mock.CheckoutBranchFn = func(name string) error { current = name; return nil }
	mock.MergeFn = func(base string) error {
		calls = append(calls, mergeCall{base, current})
		if current == "b2" {
			return assert.AnError // conflict on b2
		}
		return nil
	}
	mock.ConflictedFilesFn = func() ([]string, error) { return nil, nil }

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.ErrorIs(t, err, ErrConflict)
	assert.Contains(t, output, "gh stack pull --continue")

	stateData, readErr := os.ReadFile(filepath.Join(tmpDir, pullStateFile))
	require.NoError(t, readErr, "pull state file should be saved")

	var state pullState
	require.NoError(t, json.Unmarshal(stateData, &state))
	assert.Equal(t, "b2", state.ConflictBranch)
	assert.Equal(t, []string{"b3"}, state.RemainingBranches)
	assert.Equal(t, "b1", state.OriginalBranch)
	assert.Contains(t, state.OriginalRefs, "b1")
	assert.Contains(t, state.OriginalRefs, "b2")
	assert.Contains(t, state.OriginalRefs, "b3")
}

// TestPull_Continue_NoState verifies that --continue without a state file
// reports that no pull is in progress.
func TestPull_Continue_NoState(t *testing.T) {
	tmpDir := t.TempDir()

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b1", &calls)
	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetArgs([]string{"--continue"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.ErrorIs(t, err, ErrSilent)
	assert.Contains(t, output, "no pull in progress")
}

// TestPull_Abort_RestoresBranches verifies that --abort resets all branches to
// their original SHAs and removes the state file.
func TestPull_Abort_RestoresBranches(t *testing.T) {
	tmpDir := t.TempDir()

	state := &pullState{
		CurrentBranchIndex: 1,
		ConflictBranch:     "b2",
		RemainingBranches:  []string{"b3"},
		OriginalBranch:     "b1",
		OriginalRefs: map[string]string{
			"b1": "orig-sha-b1",
			"b2": "orig-sha-b2",
			"b3": "orig-sha-b3",
		},
	}
	stateData, _ := json.MarshalIndent(state, "", "  ")
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, pullStateFile), stateData, 0644))

	var resets []resetCall
	var checkouts []string
	current := "b2"

	var calls []mergeCall
	mock := newPullMock(tmpDir, current, &calls)
	mock.CheckoutBranchFn = func(name string) error {
		checkouts = append(checkouts, name)
		current = name
		return nil
	}
	mock.ResetHardFn = func(ref string) error {
		resets = append(resets, resetCall{current, ref})
		return nil
	}
	mock.IsMergeInProgressFn = func() bool { return true }
	mergeAborted := false
	mock.MergeAbortFn = func() error { mergeAborted = true; return nil }

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetArgs([]string{"--abort"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.NoError(t, err)
	assert.True(t, mergeAborted, "an in-progress merge should be aborted")
	assert.Contains(t, output, "Pull aborted and branches restored")

	resetMap := make(map[string]string)
	for _, r := range resets {
		resetMap[r.branch] = r.sha
	}
	assert.Equal(t, "orig-sha-b1", resetMap["b1"])
	assert.Equal(t, "orig-sha-b2", resetMap["b2"])
	assert.Equal(t, "orig-sha-b3", resetMap["b3"])

	_, statErr := os.Stat(filepath.Join(tmpDir, pullStateFile))
	assert.True(t, os.IsNotExist(statErr), "state file should be removed after abort")
	assert.Contains(t, checkouts, "b1", "should return to original branch")
}

// TestPull_ContinueCascadesRemaining verifies that --continue finalizes the
// conflicted merge and merges the remaining branches.
func TestPull_ContinueCascadesRemaining(t *testing.T) {
	s := stack.Stack{
		Trunk: stack.BranchRef{Branch: "main"},
		Branches: []stack.BranchRef{
			{Branch: "b1"},
			{Branch: "b2"},
			{Branch: "b3"},
		},
	}

	tmpDir := t.TempDir()
	writeStackFile(t, tmpDir, s)

	state := &pullState{
		CurrentBranchIndex: 1,
		ConflictBranch:     "b2",
		RemainingBranches:  []string{"b3"},
		OriginalBranch:     "b1",
		OriginalRefs: map[string]string{
			"b1": "sha-b1", "b2": "sha-b2", "b3": "sha-b3",
		},
		TrunkRef:   "main",
		StartIndex: 0,
		EndIndex:   3,
	}
	stateData, _ := json.MarshalIndent(state, "", "  ")
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, pullStateFile), stateData, 0644))

	var calls []mergeCall
	mock := newPullMock(tmpDir, "b1", &calls)
	mergeContinued := false
	mock.IsMergeInProgressFn = func() bool { return !mergeContinued }
	mock.MergeContinueFn = func() error { mergeContinued = true; return nil }

	restore := git.SetOps(mock)
	defer restore()

	cfg, _, errR := config.NewTestConfig()
	cmd := PullCmd(cfg)
	cmd.SetArgs([]string{"--continue"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()

	cfg.Err.Close()
	errOut, _ := io.ReadAll(errR)
	output := string(errOut)

	assert.NoError(t, err)
	assert.True(t, mergeContinued, "the conflicted merge should be finalized")
	require.Len(t, calls, 1, "only the remaining branch b3 should be merged")
	assert.Equal(t, mergeCall{"b2", "b3"}, calls[0])
	assert.Contains(t, output, "updated locally by merging")

	_, statErr := os.Stat(filepath.Join(tmpDir, pullStateFile))
	assert.True(t, os.IsNotExist(statErr), "state file should be cleared after continue")
}
