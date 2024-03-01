# Tests specification

The tests that `vmshed` should run are configured via a TOML file which is set
with `--tests`. This document lists the available keys.

Example configuration file: [tests.example.toml](../example/tests.example.toml).

[Test run determination](./test-run-determination.md) describes how these keys
together with the VM base image specification are used to select the test runs.

## `test_suite_file`

String. Virter provisioning file to run a test.

## `test_timeout`

String for Go's `time.ParseDuration`. Timeout for each test run.

## `artifacts`

Array of String. Paths to copy from each VM after each test run.

## `variants`

Array of Table.

### `variants.name`

String. Name of the variant. This forms part of the test run ID.

### `variants.variables.<key>`

String. The key-value pair will be passed to Virter using `--set
values.<key>=<value>` when running `test_suite_file`.

### `variants.ipv6`

Boolean. Configure IPv6 for the access network for test runs of this variant.

### `variants.vm_tags`

Array of String. Only use VM base images with corresponding `vm_tags` for test
runs of this variant.

## `networks`

Array of Table. Networks that are added to all test runs.

See `virter network add --help` for more details.

### `networks.forward`

String. Forward mode.

### `networks.ipv6`

Boolean. Configure IPv6.

### `networks.dhcp`

Boolean. Configure DHCP.

### `networks.domain`

String. Domain name for DNS.

## `tests.<test_name>`

Table. Defines a test with the given name.

### `tests.<test_name>.vms`

Array of Integer. For each value, a test run will be started with this many VMs.

### `tests.<test_name>.vm_tags`

Array of String. Only use VM base images with corresponding `vm_tags` for runs
of this test.

### `tests.<test_name>.samevms`

Boolean. Use the same VM base image for each VM in any run of this test.

### `tests.<test_name>.needallplatforms`

Boolean. Run this test once for each available VM base image.

### `tests.<test_name>.variants`

Array of String. Run this test for only these variants.

### `tests.<test_name>.networks`

Array of Table. Additional networks that are added to this test only. See
[`networks`](#networks-array-of-table) for a description of the keys.
