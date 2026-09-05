package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/modify"
	"github.com/github/gh-stack/internal/stack"
	"github.com/spf13/cobra"
)

type pullOptions struct {
	branch    string
	downstack bool
	upstack   bool
	cont      bool
	abort     bool
	noTrunk   bool
	remote    string
}

type pullState struct {
	CurrentBranchIndex int               `json:"currentBranchIndex"`
	ConflictBranch     string            `json:"conflictBranch"`
	RemainingBranches  []string          `json:"remainingBranches"`
	OriginalBranch     string            `json:"originalBranch"`
	OriginalRefs       map[string]string `json:"originalRefs"`
	NoTrunk            bool              `json:"noTrunk,omitempty"`
	TrunkRef           string            `json:"trunkRef,omitempty"`
	TrunkSHA           string            `json:"trunkSha,omitempty"`
	StartIndex         int               `json:"startIndex,omitempty"`
	EndIndex           int               `json:"endIndex,omitempty"`
}

const pullStateFile = "gh-stack-pull-state"

func PullCmd(cfg *config.Config) *cobra.Command {
	opts := &pullOptions{}

	cmd := &cobra.Command{
		Use:   "pull [branch]",
		Short: "Pull a stack of branches using merges",
		Long: `Pull from remote and propagate updates across the stack with merges.

Like ` + "`gh stack rebase`" + `, but each branch takes the tip of the previous
layer through a merge commit instead of a rebase, preserving existing history.

Use --no-trunk to skip fetching and merging the trunk branch. Only the
inter-branch merges are performed (branch 1 into branch 2, branch 2 into
branch 3, etc.).`,
		Example: `  # Pull the entire stack
  $ gh stack pull

  # Only pull from trunk to the current branch
  $ gh stack pull --downstack

  # Only pull from current branch to the top
  $ gh stack pull --upstack

  # Merge stack branches without pulling from or merging trunk
  $ gh stack pull --no-trunk

  # Continue after resolving conflicts
  $ gh stack pull --continue

  # Abort and restore all branches
  $ gh stack pull --abort`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.branch = args[0]
			}
			return runPull(cfg, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.downstack, "downstack", false, "Only pull branches from trunk to current branch")
	cmd.Flags().BoolVar(&opts.upstack, "upstack", false, "Only pull branches from current branch to top")
	cmd.Flags().BoolVar(&opts.noTrunk, "no-trunk", false, "Skip trunk — only merge stack branches into each other")
	cmd.Flags().BoolVar(&opts.cont, "continue", false, "Continue pull after resolving conflicts")
	cmd.Flags().BoolVar(&opts.abort, "abort", false, "Abort pull and restore all branches")
	cmd.Flags().StringVar(&opts.remote, "remote", "", "Remote to fetch from (defaults to auto-detected remote)")

	return cmd
}

func runPull(cfg *config.Config, opts *pullOptions) error {
	gitDir, err := git.GitDir()
	if err != nil {
		cfg.Errorf("not a git repository")
		return ErrNotInStack
	}

	if opts.cont {
		return continuePull(cfg, gitDir)
	}

	if opts.abort {
		return abortPull(cfg, gitDir)
	}

	if err := modify.CheckStateGuard(gitDir); err != nil {
		cfg.Errorf("%s", err)
		return ErrModifyRecovery
	}

	result, err := loadStack(cfg, opts.branch)
	if err != nil {
		return ErrNotInStack
	}
	sf := result.StackFile
	s := result.Stack
	currentBranch := result.CurrentBranch

	// Enable git rerere so conflict resolutions are remembered.
	if err := ensureRerere(cfg); errors.Is(err, errInterrupt) {
		return ErrSilent
	}

	var trunk trunkTarget
	if !opts.noTrunk {
		remote, err := pickRemote(cfg, currentBranch, opts.remote)
		if err != nil {
			if !errors.Is(err, errInterrupt) {
				cfg.Errorf("%s", err)
			}
			return ErrSilent
		}

		trunk, err = resolveTrunkTarget(cfg, s, remote, currentBranch)
		if err != nil {
			return err
		}

		if err := git.FetchBranches(remote, activeBranchNames(s)); err != nil {
			cfg.Errorf("failed to fetch stack branches from %s: %v", remote, err)
			return ErrSilent
		}
		fastForwardBranches(cfg, s, remote, currentBranch)
	}

	cfg.Printf("Stack detected: %s", s.DisplayChain())

	currentIdx := s.IndexOf(currentBranch)
	if currentIdx < 0 {
		currentIdx = 0
	}

	if opts.upstack && s.Branches[currentIdx].IsMerged() {
		cfg.Warningf("Current branch %q has already been merged", currentBranch)
	}

	startIdx, endIdx := computeCascadeRange(s, currentIdx, opts.downstack, opts.upstack, opts.noTrunk)

	branchesToPull := s.Branches[startIdx:endIdx]

	if len(branchesToPull) == 0 {
		cfg.Printf("No branches to pull")
		return nil
	}

	cfg.Printf("Merging branches in order, starting from %s to %s",
		branchesToPull[0].Branch, branchesToPull[len(branchesToPull)-1].Branch)

	// Sync PR state before pulling so we can detect merged and queued PRs.
	_ = syncStackPRs(cfg, s)

	originalRefs, err := resolveOriginalRefs(s)
	if err != nil {
		return fmt.Errorf("resolving branch refs: %w", err)
	}

	pullResult := cascadeMerge(cascadeRebaseOpts{
		Cfg:          cfg,
		Stack:        s,
		Branches:     branchesToPull,
		StartAbsIdx:  startIdx,
		OriginalRefs: originalRefs,
		TrunkRef:     trunk.Ref,
	})

	if pullResult.Err != nil {
		cfg.Errorf("%v", pullResult.Err)
		if pullResult.Rebased {
			restoreRebaseRefs(cfg, currentBranch, originalRefs)
		} else {
			_ = git.CheckoutBranch(currentBranch)
		}
		return ErrSilent
	}

	if pullResult.Conflicted {
		cfg.Warningf("Merging %s into %s — conflict", pullResult.ConflictBase, pullResult.ConflictBranch)

		state := &pullState{
			CurrentBranchIndex: pullResult.ConflictIdx,
			ConflictBranch:     pullResult.ConflictBranch,
			RemainingBranches:  pullResult.Remaining,
			OriginalBranch:     currentBranch,
			OriginalRefs:       originalRefs,
			NoTrunk:            opts.noTrunk,
			TrunkRef:           trunk.Ref,
			TrunkSHA:           trunk.SHA,
			StartIndex:         startIdx,
			EndIndex:           endIdx,
		}
		if err := savePullState(gitDir, state); err != nil {
			cfg.Warningf("failed to save pull state: %s", err)
		}

		printConflictDetailsWithContinue(cfg, pullResult.ConflictBranch, "gh stack pull --continue")
		cfg.Printf("")

		cfg.Printf("Resolve conflicts on %s, then run `%s`",
			pullResult.ConflictBranch, cfg.ColorCyan("gh stack pull --continue"))
		cfg.Printf("Or abort this operation with `%s`",
			cfg.ColorCyan("gh stack pull --abort"))
		return ErrConflict
	}

	_ = git.CheckoutBranch(currentBranch)

	if unstacked := verifyStacked(s, trunk.Ref, startIdx, endIdx); len(unstacked) > 0 {
		reportUnstacked(cfg, trunk.Ref, unstacked)
		if pullResult.Rebased {
			restoreRebaseRefs(cfg, currentBranch, originalRefs)
		}
		return ErrSilent
	}

	updateBaseSHAs(s)

	_ = syncStackPRs(cfg, s)

	stack.SaveNonBlocking(gitDir, sf)

	merged := s.MergedBranches()
	if len(merged) > 0 {
		names := make([]string, len(merged))
		for i, m := range merged {
			names[i] = m.Branch
		}
		cfg.Printf("Skipped %d merged %s: %s", len(merged), plural(len(merged), "branch", "branches"), strings.Join(names, ", "))
	}

	rangeDesc := "All branches in stack"
	if opts.downstack {
		rangeDesc = fmt.Sprintf("All downstack branches up to %s", currentBranch)
	} else if opts.upstack {
		rangeDesc = fmt.Sprintf("All upstack branches from %s", currentBranch)
	}

	if opts.noTrunk {
		cfg.Printf("%s updated locally by merging (without trunk)", rangeDesc)
	} else {
		cfg.Printf("%s updated locally by merging %s", rangeDesc, trunk.Describe())
	}
	cfg.Printf("To push up your changes, run `%s`",
		cfg.ColorCyan("gh stack push"))

	return nil
}

func continuePull(cfg *config.Config, gitDir string) error {
	state, err := loadPullState(gitDir)
	if err != nil {
		cfg.Errorf("no pull in progress")
		return ErrSilent
	}

	sf, err := stack.Load(gitDir)
	if err != nil {
		cfg.Errorf("failed to load stack state: %s", err)
		return ErrNotInStack
	}

	// Use the saved original branch to find the stack, since git may be in a
	// detached or merging state during an active merge.
	s, err := resolveStack(sf, state.OriginalBranch, cfg)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no stack found for branch %s", state.OriginalBranch)
	}
	trunkRef := state.TrunkRef
	if trunkRef == "" {
		trunkRef = s.Trunk.Branch
	}

	// Refresh PR state before selecting the base and cascading the remaining
	// branches. The queued flag is transient (not persisted), so it was lost
	// when the stack was reloaded from disk above. Mirrors the syncStackPRs
	// call in runPull before its cascade.
	_ = syncStackPRs(cfg, s)

	conflictBranch := state.ConflictBranch
	if conflictBranch == "" && state.CurrentBranchIndex >= 0 && state.CurrentBranchIndex < len(s.Branches) {
		conflictBranch = s.Branches[state.CurrentBranchIndex].Branch
	}

	cfg.Printf("Continuing pull of stack, resuming from %s to %s",
		conflictBranch, s.Branches[len(s.Branches)-1].Branch)

	if git.IsMergeInProgress() {
		if err := git.MergeContinue(); err != nil {
			return fmt.Errorf("merge continue failed — resolve remaining conflicts and try again: %w", err)
		}
	}

	baseBranch := effectiveParentBranch(s, state.CurrentBranchIndex, trunkRef)
	cfg.Successf("Merged %s into %s", baseBranch, conflictBranch)

	// Merge remaining branches using the shared cascade helper.
	if len(state.RemainingBranches) > 0 {
		remainingRefs := make([]stack.BranchRef, 0, len(state.RemainingBranches))
		startAbsIdx := -1
		for i, name := range state.RemainingBranches {
			idx := s.IndexOf(name)
			if idx < 0 {
				return fmt.Errorf("branch %q from saved pull state is no longer in the stack — the stack may have been modified since the pull started; consider aborting with --abort", name)
			}
			if startAbsIdx < 0 {
				startAbsIdx = idx
			} else if idx != startAbsIdx+i {
				return fmt.Errorf("branch %q is at stack index %d, expected %d — the stack may have been reordered since the pull started; consider aborting with --abort", name, idx, startAbsIdx+i)
			}
			remainingRefs = append(remainingRefs, s.Branches[idx])
		}

		result := cascadeMerge(cascadeRebaseOpts{
			Cfg:          cfg,
			Stack:        s,
			Branches:     remainingRefs,
			StartAbsIdx:  startAbsIdx,
			OriginalRefs: state.OriginalRefs,
			TrunkRef:     trunkRef,
		})

		if result.Err != nil {
			cfg.Errorf("%v", result.Err)
			restoreRebaseRefs(cfg, state.OriginalBranch, state.OriginalRefs)
			clearPullState(gitDir)
			return ErrSilent
		}

		if result.Conflicted {
			cfg.Warningf("Merging %s into %s — conflict", result.ConflictBase, result.ConflictBranch)

			state.CurrentBranchIndex = result.ConflictIdx
			state.ConflictBranch = result.ConflictBranch
			state.RemainingBranches = result.Remaining
			if err := savePullState(gitDir, state); err != nil {
				cfg.Warningf("failed to save pull state: %s", err)
			}

			printConflictDetailsWithContinue(cfg, result.ConflictBranch, "gh stack pull --continue")
			cfg.Printf("")
			cfg.Printf("Resolve conflicts on %s, then run `%s`",
				result.ConflictBranch, cfg.ColorCyan("gh stack pull --continue"))
			cfg.Printf("Or abort this operation with `%s`",
				cfg.ColorCyan("gh stack pull --abort"))
			return ErrConflict
		}
	}

	_ = git.CheckoutBranch(state.OriginalBranch)

	verifyStart, verifyEnd := state.StartIndex, state.EndIndex
	if verifyEnd <= verifyStart {
		verifyStart, verifyEnd = 0, len(s.Branches)
		if state.NoTrunk {
			verifyStart = 1
		}
	}
	if unstacked := verifyStacked(s, trunkRef, verifyStart, verifyEnd); len(unstacked) > 0 {
		reportUnstacked(cfg, trunkRef, unstacked)
		restoreRebaseRefs(cfg, state.OriginalBranch, state.OriginalRefs)
		clearPullState(gitDir)
		return ErrSilent
	}

	clearPullState(gitDir)
	updateBaseSHAs(s)

	_ = syncStackPRs(cfg, s)

	stack.SaveNonBlocking(gitDir, sf)

	if state.NoTrunk {
		cfg.Printf("All branches in stack updated locally by merging (without trunk)")
	} else if state.TrunkSHA != "" {
		cfg.Printf("All branches in stack updated locally by merging %s (%s)", trunkRef, short(state.TrunkSHA))
	} else {
		cfg.Printf("All branches in stack updated locally by merging %s", trunkRef)
	}
	cfg.Printf("To push up your changes and open/update the stack of PRs, run `%s`",
		cfg.ColorCyan("gh stack submit"))

	return nil
}

func abortPull(cfg *config.Config, gitDir string) error {
	state, err := loadPullState(gitDir)
	if err != nil {
		cfg.Errorf("no pull in progress")
		return ErrSilent
	}

	if git.IsMergeInProgress() {
		_ = git.MergeAbort()
	}

	var restoreErrors []string
	for branch, sha := range state.OriginalRefs {
		if err := git.CheckoutBranch(branch); err != nil {
			restoreErrors = append(restoreErrors, fmt.Sprintf("checkout %s: %s", branch, err))
			continue
		}
		if err := git.ResetHard(sha); err != nil {
			restoreErrors = append(restoreErrors, fmt.Sprintf("reset %s: %s", branch, err))
		}
	}

	_ = git.CheckoutBranch(state.OriginalBranch)
	clearPullState(gitDir)

	if len(restoreErrors) > 0 {
		cfg.Warningf("Pull aborted but some branches could not be fully restored:")
		for _, e := range restoreErrors {
			cfg.Printf("  %s", e)
		}
		return ErrSilent
	}

	cfg.Successf("Pull aborted and branches restored")
	return nil
}

func savePullState(gitDir string, state *pullState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("error serializing pull state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, pullStateFile), data, 0644); err != nil {
		return fmt.Errorf("error writing pull state: %w", err)
	}
	return nil
}

func loadPullState(gitDir string) (*pullState, error) {
	data, err := os.ReadFile(filepath.Join(gitDir, pullStateFile))
	if err != nil {
		return nil, err
	}
	var state pullState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func clearPullState(gitDir string) {
	_ = os.Remove(filepath.Join(gitDir, pullStateFile))
}
