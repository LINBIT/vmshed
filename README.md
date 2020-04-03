# vmshed

vmshed is a shed for storing your VMs. More precisely, it is a s(c)heduler for
running tests in VMs.

## Usage

vmshed basically takes as input two configuration files, one that defines the
tests, and one that defines the set of VMs. Then it executes tests concurrently
and collects the result and if desired prepares output that can be used in
Jenkins.

Example:

```
vmshed -tests example/tests.example.toml -vms example/vms.example.toml
```
