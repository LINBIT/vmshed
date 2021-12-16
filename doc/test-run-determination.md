# Test run determination

vmshed runs a set of tests based on the test and VM specifications. These runs
are determined as described here.

## Test run selection

The test runs are defined by taking all combinations (Cartesian product) of the
following parameters:

* Test name
  * Definition: The keys under `tests` in the test specification
  * Default: Empty set
  * Filter: `--torun` flag
* VM count
  * Definition: `vms` key in corresponding `tests` table in the test specification
  * Default: Empty set
* Variant
  * Definition: `variants` entries in the test specification
  * Default: One variant with name `default`
  * Filter: `--variant` flag; `variants` key in corresponding `tests` table in
    the test specification
* Platforms wildcard
  * Definition: When the `needallplatforms` key in corresponding `tests` table
    in the test specification is set, a test run is defined for each `vms`
    entry in the VM specification
  * Default: When `needallplatforms` is not set, one test run
  * Filter: As described in [VM selection](#vm-selection)
* Repeats
  * Definition: `--repeats` flag
  * Default: 1

The test run ID takes the form `{Test name}-{VM count}-{Variant
name}-{Counter}`. The final element `Counter` increments whenever the other
elements are the same. That is, it differentiates between runs when the
platforms wildcard is used or multiple repeats are requested.

## VM selection

VM base images must be assigned to each test run. The available base images are
defined by the `vms` entries in the VM specification. They are filtered by:

* `--base-image` flag
* The `tags` array in the `vms` entry must be a superset of `tags` in the
  `tests` table in the test specification for the test in question

The base images are assigned to the test runs as follows:

* If `needallplatforms` is set for the test, a separate test run is generated
  for each available VM base image. Each VM within a test run uses the same
  base image.
* Else, if `samevms` is set for the test, one randomly chosen base image is
  used for all the VMs of the test run.
* Otherwise, each VM for the test run is independently randomly chosen from the
  available base images.
