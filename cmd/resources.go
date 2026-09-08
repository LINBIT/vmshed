package cmd

import (
	"fmt"
	"maps"
	"net"
	"runtime"

	"github.com/rck/unit"
)

// Defaults applied when a VM does not specify memory or vcpus. These are
// passed to "virter vm run" explicitly.
const (
	defaultVMMemory = "4G"
	defaultVMVCPUs  = 4
)

// Defaults used by "virter image build" when vmshed does not pass
// --memory or --vcpus for provisioning.
const (
	defaultProvisionMemory = "1G"
	defaultProvisionVCPUs  = 1
)

// resources describes the CPU and memory demand of a set of VMs, or the
// resources available on the host. As a limit, a zero value means unlimited.
type resources struct {
	vcpus  uint
	memory uint64 // bytes
}

func (r resources) add(o resources) resources {
	return resources{
		vcpus:  r.vcpus + o.vcpus,
		memory: r.memory + o.memory,
	}
}

// fits reports whether r stays within limit. A zero value in limit means
// that this dimension is not limited.
func (r resources) fits(limit resources) bool {
	if limit.vcpus != 0 && r.vcpus > limit.vcpus {
		return false
	}
	if limit.memory != 0 && r.memory > limit.memory {
		return false
	}
	return true
}

func (r resources) String() string {
	return fmt.Sprintf("%d vCPUs, %s memory", r.vcpus, formatMemory(r.memory))
}

// limitString describes r as a limit, where a zero value means unlimited.
func (r resources) limitString() string {
	vcpus := "unlimited vCPUs"
	if r.vcpus != 0 {
		vcpus = fmt.Sprintf("%d vCPUs", r.vcpus)
	}
	memory := "unlimited memory"
	if r.memory != 0 {
		memory = formatMemory(r.memory) + " memory"
	}
	return vcpus + ", " + memory
}

// isLimited reports whether at least one dimension of the limit is set.
func (r resources) isLimited() bool {
	return r.vcpus != 0 || r.memory != 0
}

// hostCPUs returns the CPUs of the host as the default vCPU limit. Quotas
// imposed by cgroups, for example on a container, are not taken into account.
func hostCPUs() uint {
	return uint(runtime.NumCPU())
}

// memoryUnit parses the same sizes as the --memory flag of virter: the
// default units of the unit package plus the "iB" aliases added by virter.
var memoryUnit = unit.MustNewUnit(virterSizeUnits())

// binaryMemoryUnit formats sizes with binary units only. Aliases with the same
// multiplier would make the chosen unit name depend on map iteration order.
var binaryMemoryUnit = unit.MustNewUnit(map[string]int64{
	"B": 1, "K": unit.K, "M": unit.M, "G": unit.G, "T": unit.T, "P": unit.P, "E": unit.E,
})

func virterSizeUnits() map[string]int64 {
	units := maps.Clone(unit.DefaultUnits)
	for _, prefix := range []string{"K", "M", "G", "T", "P", "E"} {
		units[prefix+"iB"] = units[prefix]
	}
	return units
}

// parseMemory parses a memory size like "4G", "512MiB" or "1000MB" into
// bytes, accepting the same values as the --memory flag of virter.
func parseMemory(s string) (uint64, error) {
	value, err := memoryUnit.ValueFromString(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q: %w", s, err)
	}
	if value.Value < 0 {
		return 0, fmt.Errorf("invalid memory size %q: must not be negative", s)
	}
	return uint64(value.Value), nil
}

// formatMemory formats bytes using the largest binary unit that divides it
// evenly, so that values round trip through parseMemory.
func formatMemory(bytes uint64) string {
	if bytes == 0 {
		return "0B"
	}
	return binaryMemoryUnit.MustNewValue(int64(bytes), unit.None).String()
}

// memoryFlag is a pflag.Value for memory sizes. A zero value means unlimited
// and is shown as no default in the help output.
type memoryFlag uint64

func (m *memoryFlag) String() string {
	if *m == 0 {
		return ""
	}
	return formatMemory(uint64(*m))
}

func (m *memoryFlag) Set(s string) error {
	value, err := parseMemory(s)
	if err != nil {
		return err
	}
	*m = memoryFlag(value)
	return nil
}

func (m *memoryFlag) Type() string {
	return "memory"
}

// effectiveMemory returns the memory passed to "virter vm run" for v.
func (v *vm) effectiveMemory() string {
	if v.Memory != "" {
		return v.Memory
	}
	return defaultVMMemory
}

// effectiveVCPUs returns the vcpus passed to "virter vm run" for v.
func (v *vm) effectiveVCPUs() uint {
	if v.VCPUs != 0 {
		return v.VCPUs
	}
	return defaultVMVCPUs
}

// vmResources returns the resources that v will occupy when running.
func vmResources(v *vm) (resources, error) {
	memory, err := parseMemory(v.effectiveMemory())
	if err != nil {
		return resources{}, fmt.Errorf("VM %s: %w", v.ID(), err)
	}
	return resources{vcpus: v.effectiveVCPUs(), memory: memory}, nil
}

// sumVMResources returns the combined resources of the given VMs.
func sumVMResources(vms []vm) (resources, error) {
	var total resources
	for i := range vms {
		r, err := vmResources(&vms[i])
		if err != nil {
			return resources{}, err
		}
		total = total.add(r)
	}
	return total, nil
}

// provisionResources returns the resources occupied by one provisioning VM
// for the given specification.
func provisionResources(spec *vmSpecification) (resources, error) {
	memoryString := spec.ProvisionMemory
	if memoryString == "" {
		memoryString = defaultProvisionMemory
	}
	memory, err := parseMemory(memoryString)
	if err != nil {
		return resources{}, fmt.Errorf("provision_memory: %w", err)
	}

	vcpus := spec.ProvisionCPUs
	if vcpus == 0 {
		vcpus = defaultProvisionVCPUs
	}
	return resources{vcpus: vcpus, memory: memory}, nil
}

// smallestVMDemand returns the smallest memory and the smallest vcpus of any
// VM that may run, including the provisioning VM. The two values need not
// belong to the same VM.
func smallestVMDemand(suiteRun *testSuiteRun) resources {
	var smallest resources
	first := true
	consider := func(r resources) {
		if first {
			smallest = r
			first = false
			return
		}
		smallest.vcpus = min(smallest.vcpus, r.vcpus)
		smallest.memory = min(smallest.memory, r.memory)
	}

	for _, run := range suiteRun.testRuns {
		for i := range run.vms {
			r, err := vmResources(&run.vms[i])
			if err != nil {
				// already validated when the run was created
				continue
			}
			consider(r)
		}
	}
	if suiteRun.vmSpec.ProvisionFile != "" {
		consider(suiteRun.provisionResources)
	}
	return smallest
}

// accessNetworkHostCapacity returns how many DHCP host entries fit into an
// access network, leaving out the network, gateway and broadcast addresses.
// libvirt does not support more than 2^16-1 hosts in a DHCP range.
func accessNetworkHostCapacity(ipNet *net.IPNet) int {
	ones, bits := ipNet.Mask.Size()
	hostBits := bits - ones
	if hostBits > 16 {
		return 65534
	}
	return max(1<<hostBits-3, 1)
}

// maxVMsWithinLimit returns the number of VM IDs to use when only the
// resource limit should restrict scheduling. No more VMs than this can run
// at the same time within the limit, because every VM demands at least the
// smallest memory and vcpus. The result is capped by the host addresses of
// the access network.
func maxVMsWithinLimit(suiteRun *testSuiteRun) int {
	limit := suiteRun.resourceLimit
	smallest := smallestVMDemand(suiteRun)
	bound := uint64(accessNetworkHostCapacity(suiteRun.firstV4Net))

	if limit.memory != 0 && smallest.memory != 0 {
		bound = min(bound, limit.memory/smallest.memory)
	}
	if limit.vcpus != 0 && smallest.vcpus != 0 {
		bound = min(bound, uint64(limit.vcpus/smallest.vcpus))
	}
	return int(max(bound, 1))
}
