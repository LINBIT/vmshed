package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type vm struct {
	Name      string            `toml:"name"`
	BaseImage string            `toml:"base_image"`
	Values    map[string]string `toml:"values"`
	Memory    string            `toml:"memory"`
	VCPUs     uint              `toml:"vcpus"`
	BootCap   string            `toml:"boot_capacity"`
	Disks     []string          `toml:"disks"`
	Tags      []string          `toml:"tags"`
}

func (v *vm) ID() string {
	if v.Name != "" {
		return v.Name
	}
	return v.BaseImage
}

type vmInstance struct {
	ImageName    string
	nr           int
	memory       string
	vcpus        uint
	bootCap      string
	disks        []string
	networkNames []string
}

func (vm vmInstance) vmName() string {
	return fmt.Sprintf("lbtest-vm-%d", vm.nr)
}

func testIDString(test string, vmCount int, variantName string, testIndex int) string {
	return fmt.Sprintf("%s-%d-%s-%d", test, vmCount, variantName, testIndex)
}

func provisionImage(ctx context.Context, suiteRun *testSuiteRun, nr int, v *vm, networkName string) error {
	newImageName := suiteRun.vmSpec.ImageName(v)
	logger := log.WithFields(log.Fields{
		"Action":    "Provision",
		"ImageName": newImageName,
	})

	outDir := filepath.Join(suiteRun.outDir, "provision-log")

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "image", "rm", newImageName}
	rmStderrPath := filepath.Join(outDir, fmt.Sprintf("pre_image_rm_%s.log", newImageName))
	log.Debugf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmdStderrTerm(ctx, logger, rmStderrPath, cmd); err != nil {
		return err
	}

	argv = []string{"virter", "image", "build",
		"--id", strconv.Itoa(nr),
		"--provision", suiteRun.vmSpec.ProvisionFile,
		"--console", outDir}
	for _, override := range suiteRun.overrides {
		argv = append(argv, "--set", override)
	}
	for key, value := range v.Values {
		argv = append(argv, "--set", "values."+key+"="+value)
	}
	if suiteRun.vmSpec.ProvisionBootCap != "" {
		argv = append(argv, "--bootcap", suiteRun.vmSpec.ProvisionBootCap)
	}
	if suiteRun.vmSpec.ProvisionMemory != "" {
		argv = append(argv, "--memory", suiteRun.vmSpec.ProvisionMemory)
	}
	if suiteRun.vmSpec.ProvisionCPUs != 0 {
		argv = append(argv, "--vcpus", fmt.Sprint(suiteRun.vmSpec.ProvisionCPUs))
	}
	argv = append(argv, v.BaseImage, newImageName)

	stderrPath := filepath.Join(outDir, fmt.Sprintf("%s-provision.log", newImageName))

	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(networkName)

	provisionCtx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.vmSpec.ProvisionTimeout))
	defer cancel()

	log.Debugf("EXECUTING: %s", argv)
	start := time.Now()
	err := cmdStderrTerm(provisionCtx, logger, stderrPath, cmd)
	log.Debugf("EXECUTIONTIME: Provisioning image %s: %v", newImageName, time.Since(start))

	if ctx.Err() != nil {
		return fmt.Errorf("canceled")
	}
	if provisionCtx.Err() != nil {
		return fmt.Errorf("timeout: %w", err)
	}
	return err
}

func removeImages(outDir string, vmSpec *vmSpecification) {
	if vmSpec.ProvisionFile == "" {
		return
	}

	provisionOutDir := filepath.Join(outDir, "provision-log")

	for _, v := range vmSpec.VMs {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		newImageName := vmSpec.ImageName(&v)

		argv := []string{"virter", "image", "rm", newImageName}
		stderrPath := filepath.Join(provisionOutDir, fmt.Sprintf("image_rm_%s.log", newImageName))
		log.Debugf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmdStderrTerm(ctx, log.StandardLogger(), stderrPath, cmd); err != nil {
			log.Errorf("ERROR: Could not remove image %s %v", newImageName, err)
			dumpStderr(log.StandardLogger(), err)
			// do not return, keep going...
		}
	}
}

func startVMs(ctx context.Context, logger *log.Logger, run *testRun, testnodes ...vmInstance) error {
	var vmStartWait sync.WaitGroup
	errCh := make(chan error, len(testnodes))

	for _, vm := range testnodes {
		vmStartWait.Add(1)
		go func(vm vmInstance) {
			defer vmStartWait.Done()
			if err := runVM(ctx, logger, run, vm); err != nil {
				errCh <- err
			}
		}(vm)
	}

	vmStartWait.Wait()
	close(errCh)

	// return the first error, if any
	err := <-errCh
	return err
}

func runVM(ctx context.Context, logger *log.Logger, run *testRun, vm vmInstance) error {
	vmName := vm.vmName()

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "vm", "rm", vmName}
	rmStderrPath := filepath.Join(run.outDir, fmt.Sprintf("pre_vm_rm_%s.log", vmName))
	logger.Debugf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(vm.networkNames[0])
	if err := cmdStderrTerm(ctx, logger, rmStderrPath, cmd); err != nil {
		return err
	}

	argv = []string{"virter", "vm", "run",
		"--name", vmName,
		"--id", strconv.Itoa(vm.nr),
		"--console", run.outDir,
		"--memory", vm.memory,
		"--vcpus", strconv.Itoa(int(vm.vcpus)),
		"--bootcapacity", vm.bootCap,
	}

	for _, disks := range vm.disks {
		argv = append(argv, "--disk", disks)
	}
	for _, networkName := range vm.networkNames[1:] {
		argv = append(argv, "--nic", fmt.Sprintf("type=network,source=%s", networkName))
	}
	argv = append(argv, "--wait-ssh", vm.ImageName)

	stderrPath := filepath.Join(run.outDir, fmt.Sprintf("vm_run_%s.log", vmName))

	logger.Debugf("EXECUTING: %s", argv)
	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(vm.networkNames[0])
	return cmdStderrTerm(ctx, logger, stderrPath, cmd)
}

func shutdownVMs(logger *log.Logger, outDir string, testnodes ...vmInstance) {
	vmNames := make([]string, 0, len(testnodes))

	for _, vm := range testnodes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		vmName := vm.vmName()
		vmNames = append(vmNames, vmName)

		argv := []string{"virter", "vm", "rm", vmName}
		stderrPath := filepath.Join(outDir, fmt.Sprintf("vm_rm_%s.log", vmName))
		logger.Debugf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = virterEnv(vm.networkNames[0])
		if err := cmdStderrTerm(ctx, logger, stderrPath, cmd); err != nil {
			logger.Errorf("ERROR: Could not stop VM %s: %v", vmName, err)
			dumpStderr(logger, err)
			// do not return, keep going...
		}
	}

	// Log a line at the end so that the runtime of the commands above can be estimated
	logger.Debugf("FINISH: VMs removed: %v", strings.Join(vmNames, " "))
}

func virterEnv(networkName string) []string {
	return append(os.Environ(), fmt.Sprintf("VIRTER_LIBVIRT_NETWORK=%s", networkName), "VIRTER_LIBVIRT_STATIC_DHCP=true")
}

func dumpStderr(logger *log.Logger, err error) {
	if exitErr, ok := err.(*exec.ExitError); ok {
		fmt.Fprint(logger.Out, string(exitErr.Stderr))
	}
}
