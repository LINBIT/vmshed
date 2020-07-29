package cmd

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

type suiteState struct {
	remainingRuns map[string]bool
	freeIDs       map[int]bool
	errors        []error
}

type action struct {
	name string
	// exec carries out the action. It should block until the action is
	// finished.
	exec func(ctx context.Context, suiteRun *testSuiteRun) result
}

type result struct {
	name string
	// apply updates the state with the results of the action.
	apply func(state *suiteState)
}

func execTests(suiteRun *testSuiteRun) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()

	state := suiteState{
		remainingRuns: make(map[string]bool),
		freeIDs:       make(map[int]bool, suiteRun.nrVMs),
	}
	for _, run := range suiteRun.testRuns {
		state.remainingRuns[run.testID] = true
	}
	for i := 0; i < suiteRun.nrVMs; i++ {
		state.freeIDs[suiteRun.startVM+i] = true
	}

	scheduleLoop(ctx, cancel, suiteRun, &state)

	log.Println(suiteRun.cmdName, "EXECUTIONTIME all tests:", time.Since(start))

	nErrs := len(state.errors)
	if nErrs > 0 {
		log.Println("ERROR: Printing errors for all tests")
		for _, err := range state.errors {
			log.Println(suiteRun.cmdName, err)
			if exitErr, ok := err.(*exec.ExitError); ok {
				log.Print(string(exitErr.Stderr))
			}
		}
	}
	return nErrs, nil
}

func scheduleLoop(ctx context.Context, cancel context.CancelFunc, suiteRun *testSuiteRun, state *suiteState) {
	results := make(chan result)
	activeActions := 0

	for {
		for {
			nextAction := chooseNextAction(suiteRun, state)
			if nextAction == nil {
				break
			}

			log.Println("SCHEDULE action:", nextAction.name)
			activeActions++
			go func(a *action) {
				r := a.exec(ctx, suiteRun)
				r.name = a.name
				results <- r
			}(nextAction)
		}

		if activeActions == 0 {
			if len(state.remainingRuns) > 0 {
				state.errors = append(state.errors, fmt.Errorf("Skipped test runs: %v", state.remainingRuns))
			}
			break
		}

		log.Println("WAIT for result")
		r := <-results
		log.Println("APPLYING result for:", r.name)
		r.apply(state)
		activeActions--

		if suiteRun.failTest && state.errors != nil {
			cancel()
		}
	}
}

func chooseNextAction(suiteRun *testSuiteRun, state *suiteState) *action {
	if suiteRun.failTest && state.errors != nil {
		return nil
	}

	var bestRun *testRun
	for _, run := range suiteRun.testRuns {
		canRun := state.remainingRuns[run.testID] && len(state.freeIDs) >= len(run.vms)

		// Prefer runs that use more VMs because that will generally
		// use the available IDs more efficiently
		betterRun := bestRun == nil || len(run.vms) > len(bestRun.vms)

		if canRun && betterRun {
			runCopy := run
			bestRun = &runCopy
		}
	}

	if bestRun != nil {
		delete(state.remainingRuns, bestRun.testID)
		ids := popN(state.freeIDs, len(bestRun.vms))
		return performTestAction(*bestRun, ids)
	}

	return nil
}

func popN(m map[int]bool, n int) []int {
	ints := make([]int, 0, n)
	for id := range m {
		delete(m, id)
		ints = append(ints, id)
		if len(ints) == n {
			break
		}
	}
	return ints
}

func performTestAction(run testRun, ids []int) *action {
	return &action{
		name: fmt.Sprintf("Test %s with IDs %v", run.testID, ids),
		exec: func(ctx context.Context, suiteRun *testSuiteRun) result {
			resultLog, err := performTest(ctx, suiteRun, run, ids)
			return result{
				apply: func(state *suiteState) {
					fmt.Print(resultLog)
					if err != nil {
						state.errors = append(state.errors, err)
					}
					for _, id := range ids {
						state.freeIDs[id] = true
					}
				},
			}
		},
	}
}
