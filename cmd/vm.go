package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type vm struct {
	BaseImage string            `toml:"base_image"`
	Values    map[string]string `toml:"values"`
	Memory    string            `toml:"memory"`
	VCPUs     uint              `toml:"vcpus"`
	BootCap   string            `toml:"boot_capacity"`
	Disks     []string          `toml:"disks"`
	Tags      []string          `toml:"tags"`
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

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "image", "rm", newImageName}
	log.Debugf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmdStderrTerm(ctx, logger, cmd); err != nil {
		return err
	}

	outDir := filepath.Join(suiteRun.outDir, "provision-log")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}

	argv = []string{"virter", "image", "build",
		"--id", strconv.Itoa(nr),
		"--provision", suiteRun.vmSpec.ProvisionFile}
	if outDir != "" {
		argv = append(argv, "--console", outDir)
	}
	for _, override := range suiteRun.overrides {
		argv = append(argv, "--set", override)
	}
	for key, value := range v.Values {
		argv = append(argv, "--set", "values."+key+"="+value)
	}
	if suiteRun.vmSpec.ProvisionBootCap != "" {
		argv = append(argv, "--bootcap", suiteRun.vmSpec.ProvisionBootCap)
	}
	argv = append(argv, v.BaseImage, newImageName)

	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(networkName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	provisionCtx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.vmSpec.ProvisionTimeout))
	defer cancel()

	log.Debugf("EXECUTING: %s", argv)
	start := time.Now()
	err := cmdRunTerm(provisionCtx, logger, cmd)
	log.Debugf("EXECUTIONTIME: Provisioning image %s: %v", newImageName, time.Since(start))

	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = stderr.Bytes()
	}

	if outDir != "" {
		outPath := filepath.Join(outDir, fmt.Sprintf("%s-provision.log", newImageName))
		if logWriteErr := ioutil.WriteFile(outPath, stderr.Bytes(), 0644); logWriteErr != nil {
			return logWriteErr
		}
	}

	if ctx.Err() != nil {
		return fmt.Errorf("canceled")
	}
	if provisionCtx.Err() != nil {
		return fmt.Errorf("timeout: %w", err)
	}
	return err
}

func removeImages(vmSpec *vmSpecification) {
	if vmSpec.ProvisionFile == "" {
		return
	}

	for _, v := range vmSpec.VMs {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		newImageName := vmSpec.ImageName(&v)

		// remove with "vm rm" in case the build failed leaving the provisioning VM running
		argv := []string{"virter", "vm", "rm", newImageName}
		log.Debugf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmdStderrTerm(ctx, log.StandardLogger(), cmd); err != nil {
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
	logger.Debugf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(vm.networkNames[0])
	if err := cmdStderrTerm(ctx, logger, cmd); err != nil {
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

	logger.Debugf("EXECUTING: %s", argv)
	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(vm.networkNames[0])
	return cmdStderrTerm(ctx, logger, cmd)
}

func shutdownVMs(logger *log.Logger, testnodes ...vmInstance) error {
	for _, vm := range testnodes {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		vmName := vm.vmName()

		argv := []string{"virter", "vm", "rm", vmName}
		logger.Debugf("EXECUTING: %s", argv)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = virterEnv(vm.networkNames[0])
		if err := cmdStderrTerm(ctx, logger, cmd); err != nil {
			logger.Errorf("ERROR: Could not stop VM %s: %v", vmName, err)
			dumpStderr(logger, err)
			// do not return, keep going...
		}
	}

	return nil
}

func virterEnv(networkName string) []string {
	return append(os.Environ(), fmt.Sprintf("VIRTER_LIBVIRT_NETWORK=%s", networkName), "VIRTER_LIBVIRT_STATIC_DHCP=true")
}

// cmdStderrTerm runs a Cmd, collecting stderr and terminating gracefully
func cmdStderrTerm(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd) error {
	var out bytes.Buffer
	cmd.Stderr = &out

	err := cmdRunTerm(ctx, logger, cmd)

	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = out.Bytes()
	}

	return err
}

// cmdRunTerm runs a Cmd, terminating it gracefully when the context is done
func cmdRunTerm(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd) error {
	err := cmd.Start()
	if err != nil {
		return err
	}

	complete := make(chan struct{})
	finished := make(chan struct{})

	go handleTermination(ctx, logger, cmd, complete, finished)

	err = cmd.Wait()

	// Inform the termination handler that it can stop
	close(complete)

	// Wait for the termination handler to stop, so that the context can be
	// cancelled without risk of sending an extra signal
	<-finished

	return err
}

func handleTermination(ctx context.Context, logger log.FieldLogger, cmd *exec.Cmd, complete <-chan struct{}, finished chan<- struct{}) {
	select {
	case <-ctx.Done():
		logger.Warnln("TERMINATING: Send SIGTERM")
		cmd.Process.Signal(unix.SIGTERM)
		select {
		case <-time.After(10 * time.Second):
			logger.Errorln("TERMINATING: Send SIGKILL")
			cmd.Process.Kill()
		case <-complete:
		}
	case <-complete:
	}
	close(finished)
}

func dumpStderr(logger *log.Logger, err error) {
	if exitErr, ok := err.(*exec.ExitError); ok {
		fmt.Fprint(logger.Out, string(exitErr.Stderr))
	}
}
