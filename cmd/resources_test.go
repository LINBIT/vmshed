package cmd

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMemory(t *testing.T) {
	cases := []struct {
		input string
		want  uint64
	}{
		{"0", 0},
		{"123", 123},
		{"1B", 1},
		{"1024K", 1 << 20},
		{"512M", 512 << 20},
		{"512MiB", 512 << 20},
		{"4G", 4 << 30},
		{"4GiB", 4 << 30},
		{"1T", 1 << 40},
		{"1000kB", 1000 * 1000},
		{"4GB", 4 * 1000 * 1000 * 1000},
		{"+4G", 4 << 30},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, err := parseMemory(c.input)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestParseMemoryErrors(t *testing.T) {
	// The same inputs are rejected by the --memory flag of virter.
	invalid := []string{"", "G", "4g", "4 G", " 4G", "-4G", "4.5G", "4Gi", "99999999999999999999G"}

	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			_, err := parseMemory(input)
			assert.Error(t, err)
		})
	}
}

func TestFormatMemory(t *testing.T) {
	cases := []struct {
		input uint64
		want  string
	}{
		{0, "0B"},
		{1000, "1000B"},
		{1 << 10, "1K"},
		{4 << 30, "4G"},
		{1536 << 20, "1536M"},
		{4 * 1000 * 1000 * 1000, "3906250K"},
	}

	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			got := formatMemory(c.input)
			assert.Equal(t, c.want, got)

			parsed, err := parseMemory(got)
			require.NoError(t, err)
			assert.Equal(t, c.input, parsed, "formatted memory should round trip")
		})
	}
}

func TestMemoryFlag(t *testing.T) {
	var flag memoryFlag
	assert.Equal(t, "", flag.String(), "unset flag should show no default")

	require.NoError(t, flag.Set("8G"))
	assert.Equal(t, uint64(8<<30), uint64(flag))
	assert.Equal(t, "8G", flag.String())

	assert.Error(t, flag.Set("eight"))
}

func TestResourcesFits(t *testing.T) {
	demand := resources{vcpus: 4, memory: 4 << 30}

	assert.True(t, demand.fits(resources{}), "zero limit means unlimited")
	assert.True(t, demand.fits(resources{vcpus: 4, memory: 4 << 30}))
	assert.True(t, demand.fits(resources{vcpus: 8}), "only vCPUs limited")
	assert.True(t, demand.fits(resources{memory: 8 << 30}), "only memory limited")
	assert.False(t, demand.fits(resources{vcpus: 3}))
	assert.False(t, demand.fits(resources{memory: 2 << 30}))
	assert.False(t, demand.fits(resources{vcpus: 8, memory: 2 << 30}))

	assert.Equal(t, resources{vcpus: 6, memory: 6 << 30}, demand.add(resources{vcpus: 2, memory: 2 << 30}))
}

func TestResourcesString(t *testing.T) {
	assert.Equal(t, "4 vCPUs, 4G memory", resources{vcpus: 4, memory: 4 << 30}.String())
	assert.Equal(t, "unlimited vCPUs, unlimited memory", resources{}.limitString())
	assert.Equal(t, "16 vCPUs, unlimited memory", resources{vcpus: 16}.limitString())
	assert.Equal(t, "unlimited vCPUs, 64G memory", resources{memory: 64 << 30}.limitString())
	assert.False(t, resources{}.isLimited())
	assert.True(t, resources{vcpus: 1}.isLimited())
}

func TestVMResources(t *testing.T) {
	defaults, err := vmResources(&vm{BaseImage: "a"})
	require.NoError(t, err)
	assert.Equal(t, resources{vcpus: 4, memory: 4 << 30}, defaults, "unset VM values use the vmshed defaults")

	explicit, err := vmResources(&vm{BaseImage: "a", Memory: "512M", VCPUs: 1})
	require.NoError(t, err)
	assert.Equal(t, resources{vcpus: 1, memory: 512 << 20}, explicit)

	_, err = vmResources(&vm{BaseImage: "a", Memory: "lots"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a")

	sum, err := sumVMResources([]vm{{BaseImage: "a"}, {BaseImage: "b", Memory: "2G", VCPUs: 2}})
	require.NoError(t, err)
	assert.Equal(t, resources{vcpus: 6, memory: 6 << 30}, sum)
}

func TestProvisionResources(t *testing.T) {
	defaults, err := provisionResources(&vmSpecification{})
	require.NoError(t, err)
	assert.Equal(t, resources{vcpus: 1, memory: 1 << 30}, defaults, "unset values use the virter defaults")

	explicit, err := provisionResources(&vmSpecification{ProvisionMemory: "2G", ProvisionCPUs: 2})
	require.NoError(t, err)
	assert.Equal(t, resources{vcpus: 2, memory: 2 << 30}, explicit)

	_, err = provisionResources(&vmSpecification{ProvisionMemory: "2 gigs"})
	assert.Error(t, err)
}

func TestCheckResourceLimit(t *testing.T) {
	small := testRun{testID: "small-1-default-0", vms: []vm{{BaseImage: "a"}}, resources: resources{vcpus: 2, memory: 2 << 30}}
	large := testRun{testID: "large-2-default-0", vms: []vm{{BaseImage: "a"}, {BaseImage: "a"}}, resources: resources{vcpus: 8, memory: 16 << 30}}

	suiteRun := &testSuiteRun{
		vmSpec:             &vmSpecification{ProvisionFile: "/p"},
		testRuns:           []testRun{small, large},
		provisionResources: resources{vcpus: 1, memory: 1 << 30},
	}

	assert.NoError(t, checkResourceLimit(suiteRun), "no limit set")

	suiteRun.resourceLimit = resources{vcpus: 8, memory: 16 << 30}
	assert.NoError(t, checkResourceLimit(suiteRun), "exactly fitting")

	suiteRun.resourceLimit = resources{memory: 8 << 30}
	err := checkResourceLimit(suiteRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "large-2-default-0")
	assert.NotContains(t, err.Error(), "small-1-default-0")

	suiteRun.resourceLimit = resources{memory: 512 << 20}
	err = checkResourceLimit(suiteRun)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provisioning")

	suiteRun.vmSpec.ProvisionFile = ""
	err = checkResourceLimit(suiteRun)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "provisioning", "provisioning is irrelevant without a provision file")
	assert.Contains(t, err.Error(), "small-1-default-0")
	assert.Contains(t, err.Error(), "large-2-default-0")
}

func TestMaxVMsWithinLimit(t *testing.T) {
	_, slash24, err := net.ParseCIDR("10.224.0.0/24")
	require.NoError(t, err)
	_, slash28, err := net.ParseCIDR("10.224.0.0/28")
	require.NoError(t, err)

	small := vm{BaseImage: "small", Memory: "2G", VCPUs: 1}
	large := vm{BaseImage: "large", Memory: "4G", VCPUs: 2}

	makeSuiteRun := func(limit resources) *testSuiteRun {
		return &testSuiteRun{
			vmSpec:             &vmSpecification{},
			testRuns:           []testRun{{vms: []vm{large, large}}, {vms: []vm{small}}},
			provisionResources: resources{vcpus: 1, memory: 1 << 30},
			resourceLimit:      limit,
			firstV4Net:         slash24,
		}
	}

	assert.Equal(t, 8, maxVMsWithinLimit(makeSuiteRun(resources{memory: 16 << 30})),
		"16G allows eight of the smallest 2G VMs")
	assert.Equal(t, 4, maxVMsWithinLimit(makeSuiteRun(resources{vcpus: 4})),
		"4 vCPUs allow four of the smallest 1 vCPU VMs")
	assert.Equal(t, 4, maxVMsWithinLimit(makeSuiteRun(resources{vcpus: 4, memory: 16 << 30})),
		"the stricter dimension wins")
	assert.Equal(t, 1, maxVMsWithinLimit(makeSuiteRun(resources{memory: 3 << 30})),
		"a limit fitting only the smallest VM once")

	provisioning := makeSuiteRun(resources{memory: 16 << 30})
	provisioning.vmSpec.ProvisionFile = "/p"
	assert.Equal(t, 16, maxVMsWithinLimit(provisioning),
		"the 1G provisioning VM is the smallest demand")

	capped := makeSuiteRun(resources{vcpus: 1000})
	assert.Equal(t, 253, maxVMsWithinLimit(capped), "a /24 access network has 253 host addresses")
	capped.firstV4Net = slash28
	assert.Equal(t, 13, maxVMsWithinLimit(capped), "a /28 access network has 13 host addresses")

	assert.Equal(t, 65534, accessNetworkHostCapacity(mustParse("10.0.0.0/8")))
	assert.Equal(t, 1, accessNetworkHostCapacity(mustParse("10.0.0.0/30")))
}
