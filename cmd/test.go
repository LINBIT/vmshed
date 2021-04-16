package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apparentlymart/go-cidr/cidr"
	log "github.com/sirupsen/logrus"
)

type test struct {
	VMCount          []int       `toml:"vms"`
	Tags             []string    `toml:"tags"`
	SameVMs          bool        `toml:"samevms"`          // test need the same Distribution
	NeedAllPlatforms bool        `toml:"needallplatforms"` // test need to run on all platforms
	Variants         []string    `toml:"variants"`         // only run on given variants, if empty all
	Networks         []virterNet `toml:"networks"`         // Extra NIC to add to the VMs
}

type testRun struct {
	testName string
	testID   string
	outDir   string
	vms      []vm
	networks []virterNet
	variant  variant
}

func (t *testRun) WithTestID(name string) string {
	return fmt.Sprintf("%s-%s", t.testID, name)
}

type TestStatus string

const (
	StatusSkipped       TestStatus = "SKIPPED"
	StatusSuccess       TestStatus = "SUCCESS"
	StatusCanceled      TestStatus = "CANCELED"
	StatusFailedTimeout TestStatus = "FAILED (TIMEOUT)"
	StatusFailed        TestStatus = "FAILED"
)

type TestResulter interface {
	ExecTime() time.Duration
	Err() error
}

// collect information about individual test runs
// the interface is similar to the log package (which it also uses)
type testResult struct {
	log      bytes.Buffer // log messages of the framework (starting test, timing information,...)
	testLog  bytes.Buffer // output of the test itself ('virter vm exec' output)
	execTime time.Duration
	err      error
	timeout  bool
	status   TestStatus
}

func (r testResult) ExecTime() time.Duration {
	return r.execTime
}

func (r testResult) Err() error {
	return r.err
}

func (r testResult) String() string {
	return string(r.status)
}

func performTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, ids []int, availableNets []*net.IPNet) (string, testResult) {
	if run.outDir != "" {
		err := os.MkdirAll(run.outDir, 0755)
		if err != nil {
			return "", testResult{err: err}
		}
	}

	var vms []vmInstance
	for i, v := range run.vms {
		var memory string
		var vcpus uint
		var disks []string
		if v.Memory != "" {
			memory = v.Memory
		} else {
			memory = "4G"
		}
		if v.VCPUs != 0 {
			vcpus = v.VCPUs
		} else {
			vcpus = 4
		}
		if len(v.Disks) > 0 {
			disks = v.Disks
		} else {
			disks = []string{"name=data,size=2G,bus=scsi"}
		}
		networks := make([]string, len(run.networks))
		for i := range run.networks {
			networks[i] = run.networks[i].Name
		}
		instance := vmInstance{
			ImageName: suiteRun.vmSpec.ImageName(&v),
			nr:        ids[i],
			memory:    memory,
			vcpus:     vcpus,
			disks:     disks,
			extraNics: networks,
		}
		vms = append(vms, instance)
	}

	testRes := execTest(ctx, suiteRun, run, availableNets, vms...)

	testRes.status = StatusSuccess
	var testErr error
	if ctx.Err() != nil {
		testRes.status = StatusCanceled
		testErr = fmt.Errorf("canceled")
	} else if testRes.timeout {
		testRes.status = StatusFailedTimeout
		testErr = fmt.Errorf("timeout: %w", testRes.err)
	} else if testRes.err != nil {
		testRes.status = StatusFailed
		testErr = testRes.err
	}

	var report bytes.Buffer

	fmt.Fprintln(&report, "|===================================================================================================")
	fmt.Fprintf(&report, "| ** Results for %s - %s\n", run.testID, testRes.status)
	jobURL := os.Getenv("CI_JOB_URL")
	if jobURL != "" {
		fmt.Fprintf(&report, "| ** %s/artifacts/browse/%s\n", jobURL, run.outDir)
	}
	fmt.Fprintln(&report, "|===================================================================================================")
	logLines := strings.Split(strings.TrimSpace(testRes.log.String()), "\n")
	for _, line := range logLines {
		fmt.Fprintln(&report, "|", line)
	}

	testLog := testRes.testLog.Bytes()
	if err := ioutil.WriteFile(filepath.Join(run.outDir, "test.log"), testLog, 0644); err != nil {
		fmt.Fprintf(&report, "| FAILED to write log; suppressing original error: %v\n", testErr)
		testErr = err
	}

	resultsDir := filepath.Join(suiteRun.outDir, "test-results")
	if err := XMLLog(resultsDir, run.testID, testRes, testLog); err != nil {
		fmt.Fprintf(&report, "| FAILED to write XML log; suppressing original error: %v\n", testErr)
		testErr = err
	}
	fmt.Fprintln(&report, "|===================================================================================================")

	return report.String(), testRes
}

func execTest(ctx context.Context, suiteRun *testSuiteRun, run *testRun, availableNets []*net.IPNet, testnodes ...vmInstance) testResult {
	res := testResult{}
	logger := testLogger(&res.log, suiteRun.quiet)

	logger.Printf("EXECUTING: %s Nodes(%+v)", run.testID, testnodes)

	// Prepare networks
	err := prepareNetworks(ctx, logger, run.networks, availableNets, run.WithTestID)
	defer removeNetworks(logger, run.networks, run.WithTestID)
	if err != nil {
		res.err = err
		return res
	}

	// Start VMs
	start := time.Now()
	err = startVMs(ctx, logger, run, testnodes...)
	defer shutdownVMs(logger, run, testnodes...)
	if err != nil {
		res.err = err
		return res
	}
	logger.Printf("EXECUTIONTIME: Starting VMs: %v", time.Since(start))

	testNameEnv := fmt.Sprintf("env.TEST_NAME=%s", run.testName)
	outDirValue := fmt.Sprintf("values.OutDir=%s", run.outDir)

	argv := []string{"virter", "vm", "exec",
		"--provision", suiteRun.testSpec.TestSuiteFile,
		"--set", testNameEnv,
		"--set", outDirValue}
	for _, override := range suiteRun.overrides {
		argv = append(argv, "--set", override)
	}
	// variant variables
	for key, value := range run.variant.Variables {
		argv = append(argv, "--set", "values."+key+"="+value)
	}
	for _, vm := range testnodes {
		argv = append(argv, vm.vmName())
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv(run.WithTestID)
	cmd.Stderr = &res.testLog

	testCtx, cancel := context.WithTimeout(ctx, time.Duration(suiteRun.testSpec.TestTimeout))
	defer cancel()

	logger.Printf("EXECUTING TEST: %s", argv)
	start = time.Now()
	testErr := cmdRunTerm(testCtx, logger, cmd)
	res.execTime = time.Since(start)
	logger.Printf("EXECUTIONTIME: Running test %s: %v", run.testID, res.execTime)

	if exitErr, ok := testErr.(*exec.ExitError); ok {
		exitErr.Stderr = res.testLog.Bytes()
	}

	res.err = testErr
	res.timeout = testCtx.Err() != nil

	// copy artifacts from VMs
	for _, vm := range testnodes {
		for _, directory := range suiteRun.testSpec.Artifacts {
			// tgtPath will be /outdir/logs/{testname}/{vmname}/copy/path
			tgtPath := filepath.Join(run.outDir, vm.vmName(), filepath.Dir(directory))
			os.MkdirAll(tgtPath, 0755)
			if err := copyDir(logger, run, vm, directory, tgtPath); err != nil {
				logger.Printf("ARTIFACTCOPY: FAILED copy artifact directory %s: %s", directory, err.Error())
				dumpStderr(logger, err)
			}
		}
	}

	return res
}

// We need a mutex to serialize creating networks. This is because of https://gitlab.com/libvirt/libvirt/-/issues/78
// Basically, libvirt could potentially generate the same bridge name twice, which results in unusable networks.
var networkAddRun sync.Mutex

func prepareNetworks(ctx context.Context, logger log.FieldLogger, extraNets []virterNet, availableNets []*net.IPNet, uniqueName func(string) string) error {
	netCounter := 0
	for _, network := range append([]virterNet{accessNetwork()}, extraNets...) {
		argv := []string{"virter", "network", "add", uniqueName(network.Name)}
		if network.DHCP {
			gatewayAddress := cidr.Inc(availableNets[netCounter].IP)
			networkCidr := net.IPNet{IP: gatewayAddress, Mask: availableNets[netCounter].Mask}
			argv = append(argv, "--network-cidr", networkCidr.String(), "--dhcp")
			netCounter++
		}
		if network.ForwardMode != "" {
			argv = append(argv, "--forward-mode", network.ForwardMode)
		}
		if network.Domain != "" {
			argv = append(argv, "--domain", network.Domain)
		}
		logger.Printf("EXECUTING: %s", argv)
		networkAddRun.Lock()
		err := cmdStderrTerm(ctx, logger, exec.Command(argv[0], argv[1:]...))
		networkAddRun.Unlock()
		if err != nil {
			logger.WithError(err).Warnf("failed to create test network %s", uniqueName(network.Name))
			return err
		}
	}
	return nil
}

func removeNetworks(logger log.FieldLogger, extraNetworks []virterNet, uniqueName func(string) string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, network := range append([]virterNet{accessNetwork()}, extraNetworks...) {
		argv := []string{"virter", "network", "rm", uniqueName(network.Name)}
		logger.Printf("EXECUTING: %s", argv)
		err := cmdStderrTerm(ctx, logger, exec.Command(argv[0], argv[1:]...))
		if err != nil {
			logger.WithError(err).Warnf("failed to remove test network %s", uniqueName(network.Name))
			return err
		}
	}
	return nil
}

const testAccessNetworkName = "default"

func accessNetwork() virterNet {
	return virterNet{
		Name:        testAccessNetworkName,
		Domain:      "test",
		ForwardMode: "nat",
		DHCP:        true,
	}
}

func copyDir(logger log.FieldLogger, run *testRun, vm vmInstance, srcDir string, hostDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	args := []string{"virter", "vm", "cp", vm.vmName() + ":" + srcDir, hostDir}
	logger.Printf("EXECUTING VIRTER COPY: %s", args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = virterEnv(run.WithTestID)
	return cmdStderrTerm(ctx, logger, cmd)
}

func testLogger(out io.Writer, quiet bool) *log.Logger {
	logger := log.New()
	logger.Out = out
	logger.Formatter = &log.TextFormatter{
		DisableQuote:    true,
		TimestampFormat: "15:04:05.000",
	}

	if !quiet {
		logger.AddHook(&StandardLoggerHook{})
	}

	return logger
}

// StandardLoggerHook duplicates log messages to the standard logger
type StandardLoggerHook struct {
}

func (hook *StandardLoggerHook) Fire(entry *log.Entry) error {
	logEntry := *entry
	logEntry.Logger = log.StandardLogger()
	logEntry.Log(logEntry.Level, logEntry.Message)
	return nil
}

func (hook *StandardLoggerHook) Levels() []log.Level {
	return []log.Level{
		log.PanicLevel,
		log.FatalLevel,
		log.ErrorLevel,
		log.WarnLevel,
		log.InfoLevel,
		log.DebugLevel,
	}
}
