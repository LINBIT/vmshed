package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"

	log "github.com/sirupsen/logrus"
)

type networkStage string

const (
	networkAdd   networkStage = "Add"
	networkReady networkStage = "Ready"
	networkBusy  networkStage = "Busy"
	networkError networkStage = "Error"
)

type networkState struct {
	network  virterNet
	isAccess bool
	stage    networkStage
}

type imageStage string

const (
	imageNone      imageStage = "None"
	imageProvision imageStage = "Provision"
	imageReady     imageStage = "Ready"
	imageError     imageStage = "Error"
)

type runStage string

const (
	runNew  runStage = "New"
	runExec runStage = "Exec"
	runDone runStage = "Done"
)

type suiteState struct {
	networks   map[string]*networkState
	imageStage map[string]imageStage
	runStage   map[string]runStage
	runResults map[string]testResult
	freeIDs    map[int]bool
	freeNets   *networkList
	errors     []error
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

func runScheduler(ctx context.Context, suiteRun *testSuiteRun) map[string]testResult {
	state := initializeState(suiteRun)
	defer tearDown(state)

	scheduleLoop(ctx, suiteRun, state)

	nErrs := len(state.errors)
	if nErrs == 0 {
		log.Infoln("STATUS: All tests succeeded!")
	} else {
		log.Warnln("ERROR: Printing all errors")
		for i, err := range state.errors {
			log.Warnf("ERROR %d: %s", i, err)
			if suiteRun.printErrorDetails {
				unwrapStderr(err)
			}
		}
	}
	return state.runResults
}

func initializeState(suiteRun *testSuiteRun) *suiteState {
	netlist := NewNetworkList(suiteRun.firstNet)

	state := suiteState{
		networks:   make(map[string]*networkState),
		imageStage: make(map[string]imageStage),
		runStage:   make(map[string]runStage),
		runResults: make(map[string]testResult),
		freeIDs:    make(map[int]bool),
		freeNets:   netlist,
	}
	for _, run := range suiteRun.testRuns {
		state.runStage[run.testID] = runNew
	}

	initialImageStage := imageReady
	if suiteRun.vmSpec.ProvisionFile != "" {
		initialImageStage = imageNone
	}
	for _, v := range suiteRun.vmSpec.VMs {
		state.imageStage[v.BaseImage] = initialImageStage
	}

	for i := 0; i < suiteRun.nrVMs; i++ {
		state.freeIDs[suiteRun.startVM+i] = true
	}
	return &state
}

func scheduleLoop(ctx context.Context, suiteRun *testSuiteRun, state *suiteState) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan action)
	activeActions := 0

	for {
		for {
			if runStopping(suiteRun, state) || ctx.Err() != nil {
				break
			}

			nextAction := chooseNextAction(suiteRun, state)
			if nextAction == nil {
				break
			}

			log.Debugln("SCHEDULE: Perform action:", nextAction.name())
			nextAction.updatePre(state)
			activeActions++
			go func(a action) {
				a.exec(ctx, suiteRun)
				results <- a
			}(nextAction)
		}

		if activeActions == 0 {
			for _, run := range suiteRun.testRuns {
				if state.runStage[run.testID] != runDone {
					state.errors = append(state.errors, fmt.Errorf("Skipped test run: %s", run.testID))
				}
			}
			break
		}

		log.Debugln("SCHEDULE: Wait for result")
		r := <-results
		activeActions--
		log.Debugln("SCHEDULE: Apply result for:", r.name())
		r.updatePost(state)

		if runStopping(suiteRun, state) {
			cancel()
		}
	}
}

func tearDown(state *suiteState) {
	for networkName := range state.networks {
		err := removeNetwork(networkName)
		if err != nil {
			state.errors = append(state.errors, err)
		}
	}
}

func runStopping(suiteRun *testSuiteRun, state *suiteState) bool {
	if suiteRun.failTest && state.errors != nil {
		return true
	}

	for _, netState := range state.networks {
		if netState.stage == networkError {
			return true
		}
	}

	for _, v := range suiteRun.vmSpec.VMs {
		if state.imageStage[v.BaseImage] == imageError {
			return true
		}
	}

	return false
}

func chooseNextAction(suiteRun *testSuiteRun, state *suiteState) action {
	// Ignore IDs which are being used for provisioning when deciding which
	// test to work towards. This is necessary to ensure that larger tests
	// are preferred for efficient use of the available IDs.
	nonTestIDs := countNonTestIDs(suiteRun, state)

	var bestRun *testRun

	for i, run := range suiteRun.testRuns {
		if state.runStage[run.testID] != runNew {
			continue
		}

		if nonTestIDs < len(run.vms) {
			continue
		}

		if runBetter(state, bestRun, run) {
			bestRun = &suiteRun.testRuns[i]
		}
	}

	if bestRun != nil {
		action := nextActionRun(suiteRun, state, bestRun)
		if action != nil {
			return action
		}
	}

	if len(state.freeIDs) < 1 {
		return nil
	}

	for i, v := range suiteRun.vmSpec.VMs {
		if state.imageStage[v.BaseImage] == imageNone {
			return nextActionProvision(suiteRun, state, &suiteRun.vmSpec.VMs[i])
		}
	}

	return nil
}

// runBetter returns whether b is better than (potentially nil) run a.
func runBetter(state *suiteState, a *testRun, b testRun) bool {
	if a == nil {
		return true
	}

	// Prefer runs that use more VMs because that will generally
	// use the available IDs more efficiently
	if len(b.vms) < len(a.vms) {
		return false
	}

	if len(b.vms) > len(a.vms) {
		return true
	}

	if allImagesReady(state, a) && allNetworksReady(state, a) {
		return false
	}

	if allImagesReady(state, &b) && allNetworksReady(state, &b) {
		return true
	}

	return false
}

func allImagesReady(state *suiteState, run *testRun) bool {
	for _, v := range run.vms {
		if state.imageStage[v.BaseImage] != imageReady {
			return false
		}
	}
	return true
}

func allNetworksReady(state *suiteState, run *testRun) bool {
	networkName := findReadyNetwork(state, nil, accessNetwork(), true)
	if networkName == "" {
		return false
	}

	_, remainingNetworks := findExtraNetworks(state, run)
	return len(remainingNetworks) == 0
}

// findExtraNetworks returns the names of the ready networks and the networks which are not yet ready
func findExtraNetworks(state *suiteState, run *testRun) ([]string, []virterNet) {
	networkNames := []string{}
	remainingNetworks := []virterNet{}

	usedNetworkNames := map[string]bool{}
	for _, network := range run.networks {
		networkName := findReadyNetwork(state, usedNetworkNames, network, false)

		if networkName == "" {
			remainingNetworks = append(remainingNetworks, network)
		}

		networkNames = append(networkNames, networkName)
		usedNetworkNames[networkName] = true
	}

	return networkNames, remainingNetworks
}

func findReadyNetwork(state *suiteState, exclude map[string]bool, network virterNet, access bool) string {
	for networkName, ns := range state.networks {
		if ns.stage != networkReady {
			continue
		}

		if exclude[networkName] {
			continue
		}

		if ns.network.ForwardMode != network.ForwardMode ||
			ns.network.DHCP != network.DHCP ||
			ns.network.Domain != network.Domain {
			continue
		}

		if ns.isAccess != access {
			continue
		}

		return networkName
	}
	return ""
}

func countNonTestIDs(suiteRun *testSuiteRun, state *suiteState) int {
	nonTestIDs := suiteRun.nrVMs

	for _, run := range suiteRun.testRuns {
		if state.runStage[run.testID] == runExec {
			nonTestIDs -= len(run.vms)
		}
	}

	return nonTestIDs
}

func nextActionRun(suiteRun *testSuiteRun, state *suiteState, run *testRun) action {
	if len(state.freeIDs) < len(run.vms) {
		return nil
	}

	if !allImagesReady(state, run) {
		return nil
	}

	network := accessNetwork()
	networkName := findReadyNetwork(state, nil, network, true)
	if networkName == "" {
		return makeAddNetworkAction(state, network, true)
	}

	networkNames, remainingNetworks := findExtraNetworks(state, run)
	if len(remainingNetworks) > 0 {
		return makeAddNetworkAction(state, remainingNetworks[0], false)
	}

	ids := getIDs(suiteRun, state, len(run.vms))
	return &performTestAction{
		run:          run,
		ids:          ids,
		networkNames: append([]string{networkName}, networkNames...),
	}
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

func nextActionProvision(suiteRun *testSuiteRun, state *suiteState, v *vm) action {
	network := accessNetwork()
	networkName := findReadyNetwork(state, nil, network, true)
	if networkName == "" {
		return makeAddNetworkAction(state, network, true)
	}

	ids := getIDs(suiteRun, state, 1)
	return &provisionImageAction{v: v, id: ids[0], networkName: networkName}
}

func makeAddNetworkAction(state *suiteState, network virterNet, access bool) action {
	// Due to https://gitlab.com/libvirt/libvirt/-/issues/78 only one addNetworkAction should run at a time.
	// Basically, libvirt could potentially generate the same bridge name twice, which results in unusable networks.
	for _, ns := range state.networks {
		if ns.stage == networkAdd {
			return nil
		}
	}

	networkType := "extra"
	if access {
		networkType = "access"
	}

	return &addNetworkAction{
		networkName: fmt.Sprintf("vmshed-%d-%s", len(state.networks), networkType),
		network:     network,
		access:      access,
	}
}

func deleteAll(m map[int]bool, ints []int) {
	for _, index := range ints {
		delete(m, index)
	}
}

type performTestAction struct {
	run          *testRun
	ids          []int
	networkNames []string
	report       string
	res          testResult
}

func (a *performTestAction) name() string {
	return fmt.Sprintf("Test %s with IDs %v", a.run.testID, a.ids)
}

func (a *performTestAction) updatePre(state *suiteState) {
	state.runStage[a.run.testID] = runExec
	deleteAll(state.freeIDs, a.ids)
	for _, networkName := range a.networkNames {
		state.networks[networkName].stage = networkBusy
	}
}

func (a *performTestAction) exec(ctx context.Context, suiteRun *testSuiteRun) {
	a.report, a.res = performTest(ctx, suiteRun, a.run, a.ids, a.networkNames)
}

func (a *performTestAction) updatePost(state *suiteState) {
	if log.GetLevel() < log.DebugLevel {
		log.Infof("RESULT: %s - %s", a.run.testID, a.res.status)
	} else {
		fmt.Fprint(log.StandardLogger().Out, a.report)
	}

	state.runStage[a.run.testID] = runDone
	state.runResults[a.run.testID] = a.res
	if a.res.err != nil {
		state.errors = append(state.errors,
			fmt.Errorf("%s: %w", a.run.testID, a.res.err))
	}
	for _, networkName := range a.networkNames {
		state.networks[networkName].stage = networkReady
	}
	for _, id := range a.ids {
		state.freeIDs[id] = true
	}
}

type provisionImageAction struct {
	v           *vm
	id          int
	networkName string
	err         error
}

func (a *provisionImageAction) name() string {
	return fmt.Sprintf("Provision image %s with ID %d", a.v.BaseImage, a.id)
}

func (a *provisionImageAction) updatePre(state *suiteState) {
	state.imageStage[a.v.BaseImage] = imageProvision
	delete(state.freeIDs, a.id)
	state.networks[a.networkName].stage = networkBusy
}

func (a *provisionImageAction) exec(ctx context.Context, suiteRun *testSuiteRun) {
	a.err = provisionImage(ctx, suiteRun, a.id, a.v, a.networkName)
}

func (a *provisionImageAction) updatePost(state *suiteState) {
	state.networks[a.networkName].stage = networkReady
	state.freeIDs[a.id] = true
	if a.err == nil {
		log.Infof("STATUS: Successfully provisioned %s", a.v.BaseImage)
		state.imageStage[a.v.BaseImage] = imageReady
	} else {
		state.imageStage[a.v.BaseImage] = imageError
		state.errors = append(state.errors,
			fmt.Errorf("provision %s: %w", a.v.BaseImage, a.err))
	}
}

type addNetworkAction struct {
	networkName string
	network     virterNet
	access      bool
	ipNet       *net.IPNet
	err         error
}

func (a *addNetworkAction) name() string {
	return fmt.Sprintf("Add network %s", a.networkName)
}

func (a *addNetworkAction) updatePre(state *suiteState) {
	state.networks[a.networkName] = &networkState{
		network:  a.network,
		isAccess: a.access,
		stage:    networkAdd,
	}
	if a.network.DHCP {
		a.ipNet = state.freeNets.ReserveNext()
	}
}

func (a *addNetworkAction) exec(ctx context.Context, suiteRun *testSuiteRun) {
	dhcpCount := 0
	if a.access {
		dhcpCount = suiteRun.nrVMs
	}
	a.err = addNetwork(ctx, a.networkName, a.network, a.ipNet, suiteRun.startVM, dhcpCount)
}

func (a *addNetworkAction) updatePost(state *suiteState) {
	if a.err != nil {
		state.errors = append(state.errors,
			fmt.Errorf("add network %s: %w", a.networkName, a.err))
		state.networks[a.networkName].stage = networkError
		return
	}

	state.networks[a.networkName].stage = networkReady
}

func unwrapStderr(err error) {
	for wrappedErr := err; wrappedErr != nil; wrappedErr = errors.Unwrap(wrappedErr) {
		if exitErr, ok := wrappedErr.(*exec.ExitError); ok {
			log.Warnf("ERROR DETAILS: stderr:")
			fmt.Fprint(log.StandardLogger().Out, string(exitErr.Stderr))
		}
	}
}
