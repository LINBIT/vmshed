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

### Caching provisioned images

When the VMs specification contains a `provision_file`, vmshed builds a
provisioned image for every base image at the start of each run. With the
`--push-template` flag these images are pushed to a container registry and
reused by later runs while the provisioning inputs stay unchanged:

```
vmshed --push-template 'registry.example.com/vmshed/{{ .Image }}:latest' ...
```

vmshed passes a build ID to `virter image build` that covers the contents of
the provisioning file, the `--set` overrides and the VM values from the VMs
specification. When the registry already holds an image built from the same
base image with the same build ID, virter pulls that image instead of
provisioning a new one.

Overrides and VM values only setting values that the provisioning file does
not declare in its `[values]` section cannot affect the provisioning result.
They are excluded from the build ID, so overrides which are only relevant to
the test phase, such as a test filter, do not invalidate the cache.

Note that files referenced by the provisioning file, for example rsync
sources, are not part of the build ID. Change the provisioning file itself to
force a rebuild in this case, or run once without `--push-template`.

This requires a virter version that supports `image build --push-to`, and push
access to the registry.

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
