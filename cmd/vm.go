package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/helm/helm/pkg/strvals"
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
	VMTags    []string          `toml:"vm_tags"`
	UserName  string            `toml:"user_name"`
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
	sharedDisks  []string
	networkNames []string
	UserName     string
}

func (vm vmInstance) vmName() string {
	return fmt.Sprintf("lbtest-vm-%d", vm.nr)
}

func testIDString(test string, vmCount int, variantName string, testIndex int) string {
	return fmt.Sprintf("%s-%d-%s-%d", test, vmCount, variantName, testIndex)
}

func pullImage(ctx context.Context, suiteRun *testSuiteRun, image string, templ *template.Template) error {
	logger := log.WithFields(log.Fields{
		"Action": "Pull",
		"Image":  image,
	})

	errPath := filepath.Join(suiteRun.outDir, "provision-log", fmt.Sprintf("%s-pull.log", image))
	argv := []string{"virter", "image", "pull", image}

	if templ != nil {
		source, err := executeImageTemplate(templ, image)
		if err != nil {
			return err
		}

		argv = append(argv, source)
	}

	cmd := exec.Command(argv[0], argv[1:]...)

	log.Debugf("EXECUTING: %s", argv)
	start := time.Now()
	err := cmdStderrTerm(ctx, logger, errPath, "", cmd)
	log.Debugf("EXECUTIONTIME: Pull image %s: %v", image, time.Since(start))

	return err
}

func executeImageTemplate(templ *template.Template, image string) (string, error) {
	var buf strings.Builder
	err := templ.Execute(&buf, map[string]string{
		"Image": image,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// declaredProvisionValues returns the keys of the [values] section of the
// given provisioning file.
func declaredProvisionValues(provisionFile []byte) (map[string]bool, error) {
	var parsed struct {
		Values map[string]string `toml:"values"`
	}
	if err := toml.Unmarshal(provisionFile, &parsed); err != nil {
		return nil, err
	}

	declared := make(map[string]bool, len(parsed.Values))
	for key := range parsed.Values {
		declared[key] = true
	}
	return declared, nil
}

// overrideIsProvisionRelevant reports whether a --set override can change the
// result of provisioning. An override which only sets values that the
// provisioning file does not declare is only meaningful to the test phase.
// Overrides that cannot be analyzed are conservatively treated as relevant.
func overrideIsProvisionRelevant(override string, declared map[string]bool) bool {
	// Parse the override like virter does.
	parsed := map[string]interface{}{}
	if err := strvals.ParseIntoString(override, parsed); err != nil {
		return true
	}

	for topLevel, parsedValue := range parsed {
		if topLevel != "values" {
			return true
		}

		values, ok := parsedValue.(map[string]interface{})
		if !ok {
			return true
		}

		for key := range values {
			if declared[key] {
				return true
			}
		}
	}
	return false
}

// provisionBuildID computes the virter build ID for the provisioned image of
// v. It covers the inputs that determine the provisioning result, so that
// virter can reuse a matching image pushed by an earlier run. The base image
// content is checked separately by virter itself. Files referenced by the
// provisioning file are not covered.
func provisionBuildID(suiteRun *testSuiteRun, v *vm) (string, error) {
	provisionFile, err := os.ReadFile(suiteRun.vmSpec.ProvisionFile)
	if err != nil {
		return "", err
	}

	declared, err := declaredProvisionValues(provisionFile)
	if err != nil {
		return "", fmt.Errorf("cannot parse provision file %s: %w", suiteRun.vmSpec.ProvisionFile, err)
	}

	h := sha256.New()
	component := func(kind string, value string) {
		fmt.Fprintf(h, "%s:%d:%s\x00", kind, len(value), value)
	}

	component("provision-file", string(provisionFile))
	// Overrides are hashed in order because later ones take precedence.
	// Overrides which cannot affect provisioning are skipped, so that
	// test-phase overrides like a test filter do not invalidate the cache.
	for _, override := range suiteRun.overrides {
		if overrideIsProvisionRelevant(override, declared) {
			component("set", override)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(v.Values)) {
		if !declared[key] {
			continue
		}
		component("value-key", key)
		component("value", v.Values[key])
	}
	component("boot-capacity", suiteRun.vmSpec.ProvisionBootCap)
	component("user", v.UserName)

	return hex.EncodeToString(h.Sum(nil)), nil
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
	if err := cmdStderrTerm(ctx, logger, rmStderrPath, "", cmd); err != nil {
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
		argv = append(argv, "--boot-capacity", suiteRun.vmSpec.ProvisionBootCap)
	}
	if suiteRun.vmSpec.ProvisionMemory != "" {
		argv = append(argv, "--memory", suiteRun.vmSpec.ProvisionMemory)
	}
	if suiteRun.vmSpec.ProvisionCPUs != 0 {
		argv = append(argv, "--vcpus", fmt.Sprint(suiteRun.vmSpec.ProvisionCPUs))
	}
	/* For Windows you may want to specify
	 * user_name = "Administrator"
	 * in your vms.toml.
	 */
	if v.UserName != "" {
		argv = append(argv, "--user", v.UserName)
	}
	/* Useful for debugging - port defaults to 6000+vm_id */
	argv = append(argv, "--vnc")
	argv = append(argv, "--vnc-bind-ip", "0.0.0.0")
	if suiteRun.pushImageTemplate != nil {
		pushRef, err := executeImageTemplate(suiteRun.pushImageTemplate, newImageName)
		if err != nil {
			return err
		}
		buildID, err := provisionBuildID(suiteRun, v)
		if err != nil {
			return err
		}
		argv = append(argv, "--push-to", pushRef, "--build-id", buildID)
	}
	argv = append(argv, v.BaseImage, newImageName)

	stderrPath := filepath.Join(outDir, fmt.Sprintf("%s-provision.log", newImageName))
	metaPath := filepath.Join(outDir, fmt.Sprintf("%s-meta.json", newImageName))

	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(networkName)

	provisionCtx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.vmSpec.ProvisionTimeout))
	defer cancel()

	log.Debugf("EXECUTING: %s", argv)
	start := time.Now()
	err := cmdStderrTerm(provisionCtx, logger, stderrPath, metaPath, cmd)
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
		if err := cmdStderrTerm(ctx, log.StandardLogger(), stderrPath, "", cmd); err != nil {
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
	if err := cmdStderrTerm(ctx, logger, rmStderrPath, "", cmd); err != nil {
		return err
	}

	argv = []string{"virter", "vm", "run",
		"--name", vmName,
		"--id", strconv.Itoa(vm.nr),
		"--console", run.outDir,
		"--memory", vm.memory,
		"--vcpus", strconv.Itoa(int(vm.vcpus)),
		"--boot-capacity", vm.bootCap,
	}

	for _, disks := range vm.disks {
		argv = append(argv, "--disk", disks)
	}
	for _, sharedDisk := range vm.sharedDisks {
		argv = append(argv, "--shared-disk", sharedDisk)
	}
	for _, networkName := range vm.networkNames[1:] {
		argv = append(argv, "--nic", fmt.Sprintf("type=network,source=%s", networkName))
	}
	argv = append(argv, "--wait-ssh", vm.ImageName)

	if vm.UserName != "" {
		argv = append(argv, "--user", vm.UserName)
	}
	argv = append(argv, "--vnc")
	argv = append(argv, "--vnc-bind-ip", "0.0.0.0")

	stderrPath := filepath.Join(run.outDir, fmt.Sprintf("vm_run_%s.log", vmName))

	logger.Debugf("EXECUTING: %s", argv)
	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(vm.networkNames[0])
	return cmdStderrTerm(ctx, logger, stderrPath, "", cmd)
}

func shutdownVMs(logger *log.Logger, outDir string, res *testResult, suiteRun *testSuiteRun, testnodes ...vmInstance) {
	if res.status == StatusFailed && suiteRun.onFailure == OnFailureKeepVms {
		logger.Warn("Test failed, leaving virtual machines in place")
		logger.Info("Use \"virter vm rm ...\" when done with the VMs")
		return
	}

	if len(testnodes) == 0 {
		return
	}

	vmNames := make([]string, 0, len(testnodes))
	for _, vm := range testnodes {
		vmNames = append(vmNames, vm.vmName())
	}

	ctx, cancel := context.WithTimeout(context.Background(), suiteRun.vmRmTimeout)
	defer cancel()

	// All test nodes share the same test network, so "virter vm rm" can remove
	// them in a single call.
	argv := append([]string{"virter", "vm", "rm"}, vmNames...)
	stderrPath := filepath.Join(outDir, "vm_rm.log")
	logger.Debugf("EXECUTING: %s", argv)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(testnodes[0].networkNames[0])
	if err := cmdStderrTerm(ctx, logger, stderrPath, "", cmd); err != nil {
		logger.Errorf("ERROR: Could not remove VMs %s: %v", strings.Join(vmNames, " "), err)
		dumpStderr(logger, err)
		// do not return, keep going...
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
