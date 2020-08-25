package cmd

import (
	"fmt"
	"os/exec"
	"strconv"
	"sync"

	log "github.com/sirupsen/logrus"
)

type vm struct {
	BaseImage string            `toml:"base_image"`
	Values    map[string]string `toml:"values"`
	Memory    string            `toml:"memory"`
	VCPUs     uint              `toml:"vcpus"`
}

type vmInstance struct {
	ImageName string
	nr        int
	memory    string
	vcpus     uint
}

func (vm vmInstance) vmName() string {
	return fmt.Sprintf("lbtest-vm-%d", vm.nr)
}

func testIDString(test string, vmCount int, testIndex int) string {
	return fmt.Sprintf("%s-%d-%d", test, vmCount, testIndex)
}

func provisionImages(vmSpec *vmSpecification, overrides []string, startVM int) error {
	var provisionWait sync.WaitGroup
	errCh := make(chan error, len(vmSpec.VMs))

	for i, v := range vmSpec.VMs {
		provisionWait.Add(1)
		go func(i int, v vm) {
			defer provisionWait.Done()
			// TODO ensure we don't use more than *nrVMs when provisioning
			if err := provisionImage(vmSpec, overrides, i+startVM, v); err != nil {
				errCh <- err
			}
		}(i, v)
	}

	provisionWait.Wait()
	close(errCh)

	// return the first error, if any
	err := <-errCh
	return err
}

func provisionImage(vmSpec *vmSpecification, overrides []string, nr int, v vm) error {
	newImageName := vmSpec.ImageName(v)

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "image", "rm", newImageName}
	log.Printf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
		return err
	}

	argv = []string{"virter", "image", "build",
		"--id", strconv.Itoa(nr),
		"--provision", vmSpec.ProvisionFile}
	for _, override := range overrides {
		argv = append(argv, "--set", override)
	}
	for key, value := range v.Values {
		argv = append(argv, "--set", "values."+key+"="+value)
	}
	argv = append(argv, v.BaseImage, newImageName)

	log.Printf("EXECUTING: %s", argv)
	cmd := exec.Command(argv[0], argv[1:]...)

	// use Output to capture stderr if the exit code is non-zero
	_, err := cmd.Output()
	return err
}

func removeImages(vmSpec *vmSpecification) {
	for _, v := range vmSpec.VMs {
		newImageName := vmSpec.ImageName(v)

		argv := []string{"virter", "image", "rm", newImageName}
		log.Printf("EXECUTING: %s", argv)
		if stdouterr, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			log.Errorf("ERROR: Could not remove image %s %v: stdouterr: %s", newImageName, err, stdouterr)
			// do not return, keep going...
		}
	}
}

func startVMs(logger *log.Logger, run testRun, testnodes ...vmInstance) error {
	var vmStartWait sync.WaitGroup
	errCh := make(chan error, len(testnodes))

	for _, vm := range testnodes {
		vmStartWait.Add(1)
		go func(vm vmInstance) {
			defer vmStartWait.Done()
			if err := runVM(logger, run, vm); err != nil {
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

func runVM(logger *log.Logger, run testRun, vm vmInstance) error {
	vmName := vm.vmName()

	// clean up, should not be neccessary, but hey...
	argv := []string{"virter", "vm", "rm", vmName}
	logger.Printf("EXECUTING: %s", argv)
	// this command is idempotent, so even if it does nothing, it returns zero
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
		return err
	}

	argv = []string{"virter", "vm", "run",
		"--name", vmName,
		"--id", strconv.Itoa(vm.nr),
		"--console", run.consoleDir,
		"--memory", vm.memory,
		"--vcpus", strconv.Itoa(int(vm.vcpus)),
		"--disk", "name=data,size=2G,bus=scsi",
		"--wait-ssh",
		vm.ImageName}

	logger.Printf("EXECUTING: %s", argv)
	cmd := exec.Command(argv[0], argv[1:]...)

	// use Output to capture stderr if the exit code is non-zero
	_, err := cmd.Output()
	return err
}

func shutdownVMs(logger *log.Logger, testnodes ...vmInstance) error {
	for _, vm := range testnodes {
		vmName := vm.vmName()

		argv := []string{"virter", "vm", "rm", vmName}
		logger.Printf("EXECUTING: %s", argv)
		if stdouterr, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			logger.Errorf("ERROR: Could not stop VM %s %v: stdouterr: %s", vmName, err, stdouterr)
			// do not return, keep going...
		}
	}

	return nil
}
