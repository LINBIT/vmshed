package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	log "github.com/sirupsen/logrus"
)

type suiteState struct {
	remainingRuns     map[string]bool
	remainingImages   map[string]bool
	provisionedImages map[string]bool
	freeIDs           map[int]bool
	errors            []error
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

func runScheduler(suiteRun *testSuiteRun) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := suiteState{
		remainingRuns:     make(map[string]bool),
		remainingImages:   make(map[string]bool),
		provisionedImages: make(map[string]bool),
		freeIDs:           make(map[int]bool, suiteRun.nrVMs),
	}
	for _, run := range suiteRun.testRuns {
		state.remainingRuns[run.testID] = true
	}
	for _, v := range suiteRun.vmSpec.VMs {
		state.remainingImages[v.BaseImage] = true
	}
	for i := 0; i < suiteRun.nrVMs; i++ {
		state.freeIDs[suiteRun.startVM+i] = true
	}

	scheduleLoop(ctx, cancel, suiteRun, &state)

	nErrs := len(state.errors)
	if nErrs == 0 {
		log.Println("STATUS: All tests succeeded!")
	} else {
		log.Warnln("ERROR: Printing all errors")
		for i, err := range state.errors {
			log.Warnf("ERROR %d: %s", i, err)
			unwrapStderr(err)
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

			log.Println("SCHEDULE: Perform action:", nextAction.name)
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

		log.Println("SCHEDULE: Wait for result")
		r := <-results
		log.Println("SCHEDULE: Apply result for:", r.name)
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

	var neededVM *vm
	var bestRun *testRun

	for _, run := range suiteRun.testRuns {
		if !state.remainingRuns[run.testID] {
			continue
		}

		if suiteRun.vmSpec.ProvisionFile != "" {
			haveImages := true
			for _, v := range run.vms {
				if !state.provisionedImages[v.BaseImage] {
					if state.remainingImages[v.BaseImage] {
						vCopy := v
						neededVM = &vCopy
					}
					haveImages = false
				}
			}
			if !haveImages {
				continue
			}
		}

		if len(state.freeIDs) < len(run.vms) {
			continue
		}

		// Prefer runs that use more VMs because that will generally
		// use the available IDs more efficiently
		betterRun := bestRun == nil || len(run.vms) > len(bestRun.vms)

		if betterRun {
			runCopy := run
			bestRun = &runCopy
		}
	}

	if bestRun != nil {
		delete(state.remainingRuns, bestRun.testID)
		ids := popN(state.freeIDs, len(bestRun.vms))
		return performTestAction(*bestRun, ids)
	}

	if neededVM != nil && len(state.freeIDs) > 0 {
		delete(state.remainingImages, neededVM.BaseImage)
		ids := popN(state.freeIDs, 1)
		return provisionImageAction(neededVM, ids[0])
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
			report, err := performTest(ctx, suiteRun, run, ids)
			return result{
				apply: func(state *suiteState) {
					fmt.Fprint(log.StandardLogger().Out, report)
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

func provisionImageAction(v *vm, id int) *action {
	return &action{
		name: fmt.Sprintf("Provision image %s with ID %d", v.BaseImage, id),
		exec: func(ctx context.Context, suiteRun *testSuiteRun) result {
			err := provisionImage(suiteRun.vmSpec, suiteRun.overrides, id, v)
			return result{
				apply: func(state *suiteState) {
					state.freeIDs[id] = true
					if err == nil {
						state.provisionedImages[v.BaseImage] = true
					} else {
						state.errors = append(state.errors,
							fmt.Errorf("provision %s: %w", v.BaseImage, err))
					}
				},
			}
		},
	}
}

func unwrapStderr(err error) {
	for wrappedErr := err; wrappedErr != nil; wrappedErr = errors.Unwrap(wrappedErr) {
		if exitErr, ok := wrappedErr.(*exec.ExitError); ok {
			log.Warnf("ERROR DETAILS: stderr:\n%s", string(exitErr.Stderr))
		}
	}
}
