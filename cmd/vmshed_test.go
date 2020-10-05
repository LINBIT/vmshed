package cmd

import (
	"testing"

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
	tags = ["etcd"]

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
