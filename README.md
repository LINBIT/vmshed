# vmshed

vmshed is a shed for storing your VMs. More precisely, it is a s(c)heduler for
running tests in VMs.

## Usage

vmshed basically takes as input two configuration files, one that defines the
tests ("tests specification"), and one that defines the set of VMs ("VMs
specification"). Then it executes tests concurrently and collects the result
and if desired prepares output in the JUnit format.

Example:

```
vmshed --tests example/tests.example.toml --vms example/vms.example.toml
```

The test runs are determined based on these specification files and the command
line flags as described [here](doc/test-run-determination.md).

## Tests specification

The tests specification is a TOML file that is provided with the `--tests`
flag. It defines what tests there are and how they are run.

[Tests specification](./doc/tests-specification.md) describes the available
keys.

### Test suite file

The top level key `test_suite_file` in the tests specification references
a virter provisioning file which is run with `virter vm exec`. This executes
one test.

The environment variable `TEST_NAME` contains the name of the test to be run.

To override values in the provisioning file, use the `--set` flag.
