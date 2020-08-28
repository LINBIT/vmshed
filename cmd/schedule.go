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

type action interface {
	name() string

	// updatePre updates the state before the action starts.
	updatePre(state *suiteState)

	// exec carries out the action. It should block until the action is
	// finished.
	exec(ctx context.Context, suiteRun *testSuiteRun)

	// updatePost updates the state with the results of the action.
	updatePost(state *suiteState)
}

func runScheduler(suiteRun *testSuiteRun) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := initializeState(suiteRun)

	scheduleLoop(ctx, cancel, suiteRun, state)

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

func initializeState(suiteRun *testSuiteRun) *suiteState {
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
	return &state
}

func scheduleLoop(ctx context.Context, cancel context.CancelFunc, suiteRun *testSuiteRun, state *suiteState) {
	results := make(chan action)
	activeActions := 0

	for {
		for {
			nextAction := chooseNextAction(suiteRun, state)
			if nextAction == nil {
				break
			}

			log.Println("SCHEDULE: Perform action:", nextAction.name())
			nextAction.updatePre(state)
			activeActions++
			go func(a action) {
				a.exec(ctx, suiteRun)
				results <- a
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
		log.Println("SCHEDULE: Apply result for:", r.name())
		r.updatePost(state)
		activeActions--

		if suiteRun.failTest && state.errors != nil {
			cancel()
		}
	}
}

func chooseNextAction(suiteRun *testSuiteRun, state *suiteState) action {
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
		ids := getIDs(suiteRun, state, len(bestRun.vms))
		return &performTestAction{run: bestRun, ids: ids}
	}

	if neededVM != nil && len(state.freeIDs) > 0 {
		ids := getIDs(suiteRun, state, 1)
		return &provisionImageAction{v: neededVM, id: ids[0]}
	}

	return nil
}

func getIDs(suiteRun *testSuiteRun, state *suiteState, n int) []int {
	ids := make([]int, 0, n)
	for i := 0; i < suiteRun.nrVMs; i++ {
		id := suiteRun.startVM + i
		if state.freeIDs[id] {
			ids = append(ids, id)
			if len(ids) == n {
				break
			}
		}
	}
	return ids
}

func deleteAll(m map[int]bool, ints []int) {
	for _, index := range ints {
		delete(m, index)
	}
}

type performTestAction struct {
	run    *testRun
	ids    []int
	report string
	err    error
}

func (a *performTestAction) name() string {
	return fmt.Sprintf("Test %s with IDs %v", a.run.testID, a.ids)
}

func (a *performTestAction) updatePre(state *suiteState) {
	delete(state.remainingRuns, a.run.testID)
	deleteAll(state.freeIDs, a.ids)
}

func (a *performTestAction) exec(ctx context.Context, suiteRun *testSuiteRun) {
	a.report, a.err = performTest(ctx, suiteRun, a.run, a.ids)
}

func (a *performTestAction) updatePost(state *suiteState) {
	fmt.Fprint(log.StandardLogger().Out, a.report)
	if a.err != nil {
		state.errors = append(state.errors, a.err)
	}
	for _, id := range a.ids {
		state.freeIDs[id] = true
	}
}

type provisionImageAction struct {
	v   *vm
	id  int
	err error
}

func (a *provisionImageAction) name() string {
	return fmt.Sprintf("Provision image %s with ID %d", a.v.BaseImage, a.id)
}

func (a *provisionImageAction) updatePre(state *suiteState) {
	delete(state.remainingImages, a.v.BaseImage)
	delete(state.freeIDs, a.id)
}

func (a *provisionImageAction) exec(ctx context.Context, suiteRun *testSuiteRun) {
	a.err = provisionImage(suiteRun.vmSpec, suiteRun.overrides, a.id, a.v)
}

func (a *provisionImageAction) updatePost(state *suiteState) {
	state.freeIDs[a.id] = true
	if a.err == nil {
		state.provisionedImages[a.v.BaseImage] = true
	} else {
		state.errors = append(state.errors,
			fmt.Errorf("provision %s: %w", a.v.BaseImage, a.err))
	}
}

func unwrapStderr(err error) {
	for wrappedErr := err; wrappedErr != nil; wrappedErr = errors.Unwrap(wrappedErr) {
		if exitErr, ok := wrappedErr.(*exec.ExitError); ok {
			log.Warnf("ERROR DETAILS: stderr:")
			fmt.Fprint(log.StandardLogger().Out, string(exitErr.Stderr))
		}
	}
}
