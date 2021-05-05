package cmd

import (
	"errors"
	"net"
	"reflect"
	"testing"
)

// TestTestChooseNextAction tests the scheduling choices by running a
// scheduling loop. Actions are not executed. Instead, the results of actions
// are defined by the test.
func TestChooseNextAction(t *testing.T) {
	vm0 := vm{BaseImage: "b0"}
	vm1 := vm{BaseImage: "b1"}

	_, baseNet, err := net.ParseCIDR("10.224.0.0/24")
	if err != nil {
		t.Fatal(err)
	}

	networkName0 := "vmshed-0-access"
	networkNames0 := []string{networkName0}
	networkName1 := "vmshed-1-access"

	testRun1VM := testRun{
		testID: "t1VM",
		vms:    []vm{vm0},
	}
	testRun2VM := testRun{
		testID: "t2VM",
		vms:    []vm{vm0, vm1},
	}

	type step struct {
		result action
		// an expected action that is nil indicates that we expect the run to be stopping
		expected []action
	}

	testCases := []struct {
		name     string
		suiteRun testSuiteRun
		sequence []step
	}{
		{
			name: "prefer-larger-test",
			suiteRun: testSuiteRun{
				vmSpec:   &vmSpecification{VMs: []vm{vm0, vm1}},
				testRuns: []testRun{testRun1VM, testRun2VM},
				startVM:  5,
				nrVMs:    2,
				firstNet: baseNet,
			},
			sequence: []step{
				{
					expected: []action{accessNetworkAction(networkName0)},
				},
				{
					result:   accessNetworkAction(networkName0),
					expected: []action{&performTestAction{run: &testRun2VM, ids: []int{5, 6}, networkNames: networkNames0}},
				},
				{
					result:   &performTestAction{run: &testRun2VM, ids: []int{5, 6}, networkNames: networkNames0},
					expected: []action{&performTestAction{run: &testRun1VM, ids: []int{5}, networkNames: networkNames0}},
				},
			},
		},
		{
			name: "test-fail",
			suiteRun: testSuiteRun{
				vmSpec:   &vmSpecification{VMs: []vm{vm0, vm1}},
				testRuns: []testRun{testRun1VM, testRun2VM},
				startVM:  5,
				nrVMs:    2,
				failTest: true,
				firstNet: baseNet,
			},
			sequence: []step{
				{
					expected: []action{accessNetworkAction(networkName0)},
				},
				{
					result:   accessNetworkAction(networkName0),
					expected: []action{&performTestAction{run: &testRun2VM, ids: []int{5, 6}, networkNames: networkNames0}},
				},
				{
					result:   &performTestAction{run: &testRun2VM, ids: []int{5, 6}, networkNames: networkNames0, res: testResult{err: errors.New("test failed")}},
					expected: []action{nil},
				},
			},
		},
		{
			name: "provision",
			suiteRun: testSuiteRun{
				vmSpec:   &vmSpecification{ProvisionFile: "/p", VMs: []vm{vm0, vm1}},
				testRuns: []testRun{testRun1VM, testRun2VM},
				startVM:  5,
				nrVMs:    2,
				firstNet: baseNet,
			},
			sequence: []step{
				{
					expected: []action{accessNetworkAction(networkName0)},
				},
				{
					result: accessNetworkAction(networkName0),
					expected: []action{
						&provisionImageAction{v: &vm0, id: 5, networkName: networkName0},
						accessNetworkAction(networkName1),
					},
				},
				{
					result:   accessNetworkAction(networkName1),
					expected: []action{&provisionImageAction{v: &vm1, id: 6, networkName: networkName1}},
				},
				{
					result: &provisionImageAction{v: &vm0, id: 5, networkName: networkName0},
					// larger tests are preferred, so do not expect testRun1VM to run yet
				},
				{
					result:   &provisionImageAction{v: &vm1, id: 6, networkName: networkName1},
					expected: []action{&performTestAction{run: &testRun2VM, ids: []int{5, 6}, networkNames: networkNames0}},
				},
				{
					result:   &performTestAction{run: &testRun2VM, ids: []int{5, 6}, networkNames: networkNames0},
					expected: []action{&performTestAction{run: &testRun1VM, ids: []int{5}, networkNames: networkNames0}},
				},
			},
		},
		{
			name: "provision-fail",
			suiteRun: testSuiteRun{
				vmSpec:   &vmSpecification{ProvisionFile: "/p", VMs: []vm{vm0, vm1}},
				testRuns: []testRun{testRun1VM, testRun2VM},
				startVM:  5,
				nrVMs:    2,
				firstNet: baseNet,
			},
			sequence: []step{
				{
					expected: []action{accessNetworkAction(networkName0)},
				},
				{
					result: accessNetworkAction(networkName0),
					expected: []action{
						&provisionImageAction{v: &vm0, id: 5, networkName: networkName0},
						accessNetworkAction(networkName1),
					},
				},
				{
					result:   accessNetworkAction(networkName1),
					expected: []action{&provisionImageAction{v: &vm1, id: 6, networkName: networkName1}},
				},
				{
					result: &provisionImageAction{v: &vm0, id: 5, networkName: networkName0},
					// larger tests are preferred, so do not expect testRun1VM to run yet
				},
				{
					result: &provisionImageAction{v: &vm1, id: 6, networkName: networkName0, err: errors.New("provision fail")},
					// even though testRun1VM could run, expect suite run to stop due to provisioning failure
					expected: []action{nil},
				},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			state := initializeState(&test.suiteRun)
			for _, step := range test.sequence {
				if step.result != nil {
					t.Logf("simulate completed action: '%s'", step.result.name())
					step.result.updatePost(state)
				}

				for _, expected := range step.expected {
					stopping := runStopping(&test.suiteRun, state)
					if expected == nil {
						if !stopping {
							t.Fatalf("test run not stopping as expected")
						}
						break
					}

					t.Logf("expected action: '%s'", expected.name())

					if stopping {
						t.Fatalf("test run stopping unexpectedly")
					}

					actual := chooseNextAction(&test.suiteRun, state)

					if actual == nil {
						t.Fatalf("action missing, expected '%s'", expected.name())
					}

					validateAction(t, actual, expected)
					actual.updatePre(state)
				}

				if !runStopping(&test.suiteRun, state) {
					a := chooseNextAction(&test.suiteRun, state)
					if a != nil {
						t.Fatalf("unexpected action, actual: '%s'", a.name())
					}
				}
			}
		})
	}
}

func validateAction(t *testing.T, a, e action) {
	switch expected := e.(type) {
	case *performTestAction:
		actual, ok := a.(*performTestAction)
		if !ok {
			t.Fatalf("action type does not match, expected '%v', actual '%v'", reflect.TypeOf(expected), reflect.TypeOf(a))
		}

		if actual.run.testID != expected.run.testID {
			t.Errorf("test name does not match, expected: '%s', actual: '%s'", expected.run.testID, actual.run.testID)
		}

		if !reflect.DeepEqual(actual.ids, expected.ids) {
			t.Errorf("test uses wrong IDs, expected: '%v', actual: '%v'", expected.ids, actual.ids)
		}

		if !reflect.DeepEqual(actual.networkNames, expected.networkNames) {
			t.Errorf("test uses wrong networks, expected: '%v', actual: '%v'", expected.networkNames, actual.networkNames)
		}
	case *provisionImageAction:
		actual, ok := a.(*provisionImageAction)
		if !ok {
			t.Fatalf("action type does not match, expected '%v', actual '%v'", reflect.TypeOf(expected), reflect.TypeOf(a))
		}

		if actual.v.BaseImage != expected.v.BaseImage {
			t.Errorf("VM base image does not match, expected: '%s', actual: '%s'", expected.v.BaseImage, actual.v.BaseImage)
		}

		if actual.id != expected.id {
			t.Errorf("provisioning uses wrong ID, expected: '%d', actual: '%d'", expected.id, actual.id)
		}

		if actual.networkName != expected.networkName {
			t.Errorf("provisioning uses wrong network, expected: '%s', actual: '%s'", expected.networkName, actual.networkName)
		}
	case *addNetworkAction:
		actual, ok := a.(*addNetworkAction)
		if !ok {
			t.Fatalf("action type does not match, expected '%v', actual '%v'", reflect.TypeOf(expected), reflect.TypeOf(a))
		}

		if actual.networkName != expected.networkName {
			t.Errorf("network name does not match, expected: '%s', actual: '%s'", expected.networkName, actual.networkName)
		}

		if actual.network.Domain != expected.network.Domain {
			t.Errorf("network domain name does not match, expected: '%s', actual: '%s'", expected.network.Domain, actual.network.Domain)
		}
	default:
		t.Fatalf("unhandled expected action type: '%v'", reflect.TypeOf(e))
	}
}

func accessNetworkAction(name string) action {
	return &addNetworkAction{networkName: name, network: accessNetwork()}
}
