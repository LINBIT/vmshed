package cmd

import (
	"errors"
	"reflect"
	"testing"
)

// TestTestChooseNextAction tests the scheduling choices by running a
// scheduling loop. Actions are not executed. Instead, the results of actions
// are defined by the test.
func TestChooseNextAction(t *testing.T) {
	vm0 := vm{BaseImage: "b0"}
	vm1 := vm{BaseImage: "b1"}

	testRun1VM := testRun{
		testID: "t1VM",
		vms:    []vm{vm0},
	}
	testRun2VM := testRun{
		testID: "t2VM",
		vms:    []vm{vm0, vm1},
	}

	type step struct {
		result   action
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
			},
			sequence: []step{
				{
					expected: []action{&performTestAction{run: &testRun2VM, ids: []int{5, 6}}},
				},
				{
					result:   &performTestAction{run: &testRun2VM, ids: []int{5, 6}},
					expected: []action{&performTestAction{run: &testRun1VM, ids: []int{5}}},
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
			},
			sequence: []step{
				{
					expected: []action{&performTestAction{run: &testRun2VM, ids: []int{5, 6}}},
				},
				{
					result: &performTestAction{run: &testRun2VM, ids: []int{5, 6}, err: errors.New("test failed")},
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
			},
			sequence: []step{
				{
					expected: []action{
						&provisionImageAction{v: &vm1, id: 5},
						&provisionImageAction{v: &vm0, id: 6},
					},
				},
				{
					result:   &provisionImageAction{v: &vm0, id: 6},
					expected: []action{&performTestAction{run: &testRun1VM, ids: []int{6}}},
				},
				{
					result: &provisionImageAction{v: &vm1, id: 5},
				},
				{
					result:   &performTestAction{run: &testRun1VM, ids: []int{6}},
					expected: []action{&performTestAction{run: &testRun2VM, ids: []int{5, 6}}},
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
					t.Logf("expected action: '%s'", expected.name())

					actual := chooseNextAction(&test.suiteRun, state)

					if actual == nil {
						t.Fatalf("action missing, expected '%s'", expected.name())
					}

					validateAction(t, actual, expected)
					actual.updatePre(state)
				}

				a := chooseNextAction(&test.suiteRun, state)
				if a != nil {
					t.Fatalf("unexpected action, actual: '%s'", a.name())
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
	default:
		t.Fatalf("unhandled expected action type: '%v'", reflect.TypeOf(e))
	}
}
