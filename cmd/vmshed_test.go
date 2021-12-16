package cmd

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestLoadTestsToml(t *testing.T) {
	testsToml := `test_suite_file = "run.toml"
	artifacts = ["/var/log/linstor"]

	[[variants]]
	name = "default"
	etcd = "false"

	[[variants]]
	name = "etcd"
	variables = {etcd = "true"}

	[tests]
	[tests.test_recreate_deleted_resource]
	vms = [1]

	[tests.test_migrate_etcd]
	vms = [2]
	vm_tags = ["etcd"]

	[tests.add-connect-delete]
	vms = [2]
`

	var testSpec testSpecification
	if _, err := toml.Decode(testsToml, &testSpec); err != nil {
		t.Fatal(err)
	}

	if len(testSpec.Variants) != 2 {
		t.Errorf("Wrong Variants count")
	}

	if testSpec.Variants[0].Name != "default" {
		t.Errorf("variant[0].name != default: %s", testSpec.Variants[0].Name)
	}

	if testSpec.Variants[1].Variables["etcd"] != "true" {
		t.Errorf("variant['etcd'] variable not true")
	}

	_, okRec := testSpec.Tests["test_recreate_deleted_resource"]
	_, okMigrate := testSpec.Tests["test_migrate_etcd"]
	if !okRec || !okMigrate {
		t.Errorf("tests missing")
	}
}

// tests and allowed images
type testSchedules map[string][]string

const vmSpecToml = `name = "t"
provision_file = "provision-test.toml"

[[vms]]
base_image = "centos-8-linstor-k193"
vm_tags = ["postgresql", "mariadb"]

[[vms]]
base_image = "ubuntu-xenial-linstor-k185"

[[vms]]
base_image = "ubuntu-bionic-linstor-k109"
vm_tags = ["zfs", "postgresql", "mariadb"]

[[vms]]
base_image = "ubuntu-focal-linstor-k40"
vm_tags = ["zfs", "postgresql", "mariadb"]
`

func TestDeterminedTests(t *testing.T) {
	testCases := []struct {
		name     string
		vmSpec   string
		testSpec string
		toRun    string
		repeats  int
		variants []string
		testIds  testSchedules
	}{
		{
			name:   "simpleCountTags",
			vmSpec: vmSpecToml,
			testSpec: `test_suite_file = "run.toml"

			[tests]
			[tests.test_list_commands]
			vms = [1, 2]

			[tests.test_zfs_disk2_diskless1]
			vms = [3]
			vm_tags = ['zfs']`,
			repeats: 1,
			testIds: testSchedules{
				"test_list_commands-1-default-0": []string{},
				"test_list_commands-2-default-0": []string{},
				"test_zfs_disk2_diskless1-3-default-0": []string{
					"ubuntu-bionic-linstor-k109", "ubuntu-focal-linstor-k40"},
			},
		},
		{
			name:   "repeats",
			vmSpec: vmSpecToml,
			testSpec: `test_suite_file = "run.toml"

			[tests]
			[tests.test_list_commands]
			vms = [1, 2]`,
			repeats: 3,
			testIds: testSchedules{
				"test_list_commands-1-default-0": []string{},
				"test_list_commands-1-default-1": []string{},
				"test_list_commands-1-default-2": []string{},
				"test_list_commands-2-default-0": []string{},
				"test_list_commands-2-default-1": []string{},
				"test_list_commands-2-default-2": []string{},
			},
		},
		{
			name:   "toRun",
			vmSpec: vmSpecToml,
			testSpec: `test_suite_file = "run.toml"

			[tests]
			[tests.test_list_commands]
			vms = [1, 2]

			[tests.test_recreate_deleted_resource]
			vms = [1]

			[tests.test_auto_place_replicas_on_same]
			vms = [4]

			[tests.test_size_volume_definition]
			vms = [3]

			[tests.test_zfs_disk2_diskless1]
			vms = [3]
			vm_tags = ['zfs']`,
			repeats: 1,
			toRun:   "test_list_commands,test_auto_place_replicas_on_same",
			testIds: testSchedules{
				"test_list_commands-1-default-0":               []string{},
				"test_list_commands-2-default-0":               []string{},
				"test_auto_place_replicas_on_same-4-default-0": []string{},
			},
		},
		{
			name:   "simpleVariants",
			vmSpec: vmSpecToml,
			testSpec: `test_suite_file = "run.toml"

			[[variants]]
			name = "default"
			variables = {etcd = "false"}

			[[variants]]
			name = "etcd"
			variables = {etcd = "true"}

			[tests]
			[tests.test_list_commands]
			vms = [1, 2]`,
			repeats: 1,
			testIds: testSchedules{
				"test_list_commands-1-default-0": []string{},
				"test_list_commands-1-etcd-0":    []string{},
				"test_list_commands-2-default-0": []string{},
				"test_list_commands-2-etcd-0":    []string{},
			},
		},
		{
			name:   "variantsFiltered",
			vmSpec: vmSpecToml,
			testSpec: `test_suite_file = "run.toml"

			[[variants]]
			name = "default"
			variables = {etcd = "false"}

			[[variants]]
			name = "etcd"
			variables = {etcd = "true"}

			[tests]
			[tests.test_list_commands]
			vms = [1, 2]`,
			repeats:  1,
			variants: []string{"etcd"},
			testIds: testSchedules{
				"test_list_commands-1-etcd-0": []string{},
				"test_list_commands-2-etcd-0": []string{},
			},
		},
		{
			name:   "variantVMTags",
			vmSpec: vmSpecToml,
			testSpec: `test_suite_file = "run.toml"

			[[variants]]
			name = "default"

			[[variants]]
			name = "etcd"
			vm_tags = ["zfs"]

			[tests]
			[tests.test_list_commands]
			vms = [1]
			needallplatforms = true`,
			repeats:  1,
			variants: []string{},
			testIds: testSchedules{
				"test_list_commands-1-default-0": []string{"centos-8-linstor-k193"},
				"test_list_commands-1-default-1": []string{"ubuntu-xenial-linstor-k185"},
				"test_list_commands-1-default-2": []string{"ubuntu-bionic-linstor-k109"},
				"test_list_commands-1-default-3": []string{"ubuntu-focal-linstor-k40"},
				"test_list_commands-1-etcd-0":    []string{"ubuntu-bionic-linstor-k109"},
				"test_list_commands-1-etcd-1":    []string{"ubuntu-focal-linstor-k40"},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			var vmSpec vmSpecification
			if _, err := toml.Decode(test.vmSpec, &vmSpec); err != nil {
				t.Fatal(err)
			}
			vmSpec.ProvisionFile = joinIfRel("/tmp", vmSpec.ProvisionFile)
			vmSpec.ProvisionTimeout = durationDefault(vmSpec.ProvisionTimeout, 3*time.Minute)
			vmSpec.VMs = filterVMs(vmSpec.VMs, []string{})

			var testSpec testSpecification
			if _, err := toml.Decode(test.testSpec, &testSpec); err != nil {
				t.Fatal(err)
			}
			testSpec.TestSuiteFile = joinIfRel("/tmp", testSpec.TestSuiteFile)
			testSpec.TestTimeout = durationDefault(testSpec.TestTimeout, 5*time.Minute)

			testSuiteRun, err := createTestSuiteRun(
				vmSpec, testSpec, test.toRun, "", test.repeats, test.variants)
			if err != nil {
				t.Fatal(err)
			}

			// copy
			missingTests := testSchedules{}
			for k, v := range test.testIds {
				missingTests[k] = v
			}

			for _, tr := range testSuiteRun.testRuns {
				if testSched, ok := test.testIds[tr.testID]; !ok {
					t.Errorf("Test id '%s' not scheduled", tr.testID)
				} else {
					if len(testSched) > 0 {
						for _, vm := range tr.vms {
							if !containsString(testSched, vm.ID()) {
								t.Fatalf("Base %s not allowed for test %s", vm.ID(), tr.testID)
							}
						}
					}
					delete(missingTests, tr.testID)
				}
			}

			if len(missingTests) > 0 {
				t.Errorf("Expected following tests to schedule: %v", missingTests)
			}
		})
	}
}
