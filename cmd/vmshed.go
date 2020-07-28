package cmd

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nightlyone/lockfile"
	"github.com/spf13/cobra"

	"github.com/LINBIT/vmshed/cmd/config"
)

type vmSpecification struct {
	Name          string `toml:"name"`
	ProvisionFile string `toml:"provision_file"`
	VMs           []vm   `toml:"vms"`
}

func (s *vmSpecification) ImageName(v vm) string {
	if s.ProvisionFile == "" {
		// No provisioning, use base image directly
		return v.BaseImage
	}
	return fmt.Sprintf("%s-%s", v.BaseImage, s.Name)
}

type testSpecification struct {
	TestSuiteFile string      `toml:"test_suite_file"`
	TestGroups    []testGroup `toml:"group"`
}

type testSuiteRun struct {
	cmdName     string
	vmSpec      *vmSpecification
	testSpec    *testSpecification
	overrides   []string
	jenkins     *Jenkins
	testRuns    []testRun
	nrVMs       int
	failTest    bool
	quiet       bool
	testTimeout time.Duration
}

// Execute runs vmshed
func Execute() {
	if err := rootCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}

func rootCommand() *cobra.Command {
	cmdName := filepath.Base(os.Args[0])
	prog := path.Base(os.Args[0])

	var vmSpecPath string
	var testSpecPath string
	var provisionOverrides []string
	var baseImages []string
	var toRun string
	var startVM int
	var nrVMs int
	var failTest bool
	var quiet bool
	var jenkinsWS string
	var testTimeout time.Duration
	var version bool

	rootCmd := &cobra.Command{
		Use:   "vmshed",
		Short: "Run tests in VMs",
		Long:  `Run tests in VMs`,
		Run: func(cmd *cobra.Command, args []string) {
			if version {
				fmt.Println(prog, config.Version)
				return
			}

			if startVM <= 0 {
				log.Fatal(cmdName, "--startvm has to be positive")
			}
			if nrVMs <= 0 {
				log.Fatal(cmdName, "--nvms has to be positive")
			}

			jenkins := NewJenkinsMust(jenkinsWS)

			var vmSpec vmSpecification
			if _, err := toml.DecodeFile(vmSpecPath, &vmSpec); err != nil {
				log.Fatal(err)
			}
			vmSpec.ProvisionFile = joinIfRel(filepath.Dir(vmSpecPath), vmSpec.ProvisionFile)
			vmSpec.VMs = filterVMs(vmSpec.VMs, baseImages)

			var testSpec testSpecification
			if _, err := toml.DecodeFile(testSpecPath, &testSpec); err != nil {
				log.Fatal(err)
			}
			testSpec.TestSuiteFile = joinIfRel(filepath.Dir(testSpecPath), testSpec.TestSuiteFile)

			var tests []testGroup
			for _, test := range testSpec.TestGroups {
				if toRun != "all" && toRun != "" { //filter tests
					idx := 0
					for _, tn := range test.Tests {
						for _, vt := range strings.Split(toRun, ",") {
							if tn == vt {
								test.Tests[idx] = tn
								idx++
							}
						}
					}
					test.Tests = test.Tests[:idx]
				}

				if len(test.Tests) == 0 {
					continue
				}

				tests = append(tests, test)
			}
			testSpec.TestGroups = tests

			testRuns, err := determineAllTestRuns(jenkins, &vmSpec, tests)
			if err != nil {
				log.Fatal(err)
			}

			for _, run := range testRuns {
				baseImages := make([]string, len(run.vms))
				for i, v := range run.vms {
					baseImages[i] = v.BaseImage
				}
				baseImageString := strings.Join(baseImages, ",")
				log.Printf("WILL RUN: %s with base images %s\n", run.testID, baseImageString)
			}

			start := time.Now()

			suiteRun := testSuiteRun{
				cmdName:     cmdName,
				vmSpec:      &vmSpec,
				testSpec:    &testSpec,
				overrides:   provisionOverrides,
				jenkins:     jenkins,
				testRuns:    testRuns,
				nrVMs:       nrVMs,
				failTest:    failTest,
				quiet:       quiet,
				testTimeout: testTimeout,
			}
			nFailed := provisionAndExec(&suiteRun, startVM)

			log.Println(cmdName, "OVERALL EXECUTIONTIME:", time.Since(start))

			// transfer ownership to Jenkins, so that the workspace can be cleaned before running again
			if err := jenkins.OwnWorkspace(); err != nil {
				log.Println(cmdName, "ERROR SETTING WORKSPACE OWNERSHIP:", err)
			}

			os.Exit(nFailed)
		},
	}

	rootCmd.Flags().StringVarP(&vmSpecPath, "vms", "", "vms.toml", "File containing VM specification")
	rootCmd.Flags().StringVarP(&testSpecPath, "tests", "", "tests.toml", "File containing test specification")
	rootCmd.Flags().StringSliceVarP(&provisionOverrides, "set", "s", []string{}, "set/override provisioning steps, for example '--set values.X=y'")
	rootCmd.Flags().StringSliceVarP(&baseImages, "base-image", "", []string{}, "VM base images to use (defaults to all)")
	rootCmd.Flags().StringVarP(&toRun, "torun", "", "all", "comma separated list of test names to execute ('all' is a reserved test name)")
	rootCmd.Flags().IntVarP(&startVM, "startvm", "", 2, "Number of the first VM to start in parallel")
	rootCmd.Flags().IntVarP(&nrVMs, "nvms", "", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	rootCmd.Flags().BoolVarP(&failTest, "failtest", "", false, "Stop executing tests when the first one failed")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "", false, "Don't print progess messages while tests are running")
	rootCmd.Flags().StringVarP(&jenkinsWS, "jenkins", "", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	rootCmd.Flags().DurationVarP(&testTimeout, "testtime", "", 5*time.Minute, "Timeout for a single test execution in a VM")
	rootCmd.Flags().BoolVarP(&version, "version", "", false, "Print version and exit")

	return rootCmd
}

func determineAllTestRuns(jenkins *Jenkins, vmSpec *vmSpecification, testGroups []testGroup) ([]testRun, error) {
	testRuns := []testRun{}
	for _, testGrp := range testGroups {
		for _, t := range testGrp.Tests {
			runs, err := determineRunsForTest(jenkins, vmSpec, testGrp, t)
			if err != nil {
				return nil, err
			}
			testRuns = append(testRuns, runs...)
		}
	}
	return testRuns, nil
}

func determineRunsForTest(jenkins *Jenkins, vmSpec *vmSpecification, testGrp testGroup, testName string) ([]testRun, error) {
	testRuns := []testRun{}

	needsSameVMs := false
	for _, s := range testGrp.SameVMs {
		if s == testName {
			needsSameVMs = true
			break
		}
	}

	needsAllPlatforms := false
	for _, a := range testGrp.NeedAllPlatforms {
		if a == testName {
			needsAllPlatforms = true
			break
		}
	}

	if needsAllPlatforms {
		for platformIdx, v := range vmSpec.VMs {
			testRuns = append(testRuns, newTestRun(
				jenkins, testName, repeatVM(v, testGrp.NrVMs), platformIdx))
		}
	} else if needsSameVMs {
		v, err := randomVM(vmSpec.VMs)
		if err != nil {
			return nil, err
		}
		testRuns = append(testRuns, newTestRun(
			jenkins, testName, repeatVM(v, testGrp.NrVMs), 0))
	} else {
		var vms []vm
		for i := 0; i < testGrp.NrVMs; i++ {
			v, err := randomVM(vmSpec.VMs)
			if err != nil {
				return nil, err
			}
			vms = append(vms, v)
		}
		testRuns = append(testRuns, newTestRun(jenkins, testName, vms, 0))
	}

	return testRuns, nil
}

func repeatVM(v vm, count int) []vm {
	vms := make([]vm, count)
	for i := 0; i < count; i++ {
		vms[i] = v
	}
	return vms
}

func randomVM(vms []vm) (vm, error) {
	r, err := rand.Int(rand.Reader, big.NewInt(int64(len(vms))))
	if err != nil {
		return vm{}, err
	}
	return vms[r.Int64()], nil
}

func newTestRun(jenkins *Jenkins, testName string, vms []vm, platformIdx int) testRun {
	run := testRun{
		testName: testName,
		testID:   testIDString(testName, len(vms), platformIdx),
		vms:      vms,
	}

	if jenkins.IsActive() {
		run.testDirOut = filepath.Join("log", run.testID)
		run.consoleDir = jenkins.SubDir(run.testDirOut)
	}

	return run
}

func provisionAndExec(suiteRun *testSuiteRun, startVM int) int {
	nrPool := make(chan int, suiteRun.nrVMs)
	for i := 0; i < suiteRun.nrVMs; i++ {
		nr := i + startVM
		nrPool <- nr

		lockName := fmt.Sprintf("%s.vm-%d.lock", suiteRun.cmdName, nr)
		lock, err := lockfile.New(filepath.Join(os.TempDir(), lockName))
		if err != nil {
			log.Fatalf("Cannot init lock. reason: %v", err)
		}
		if err = lock.TryLock(); err != nil {
			log.Fatalf("Cannot lock %q, reason: %v", lock, err)
		}
		defer lock.Unlock()
	}

	// Workaround: When virter first starts it generates a key pair.
	// However, when we start multiple instances concurrently, they race.
	// The result is that the VMs start successfully, but then the test can
	// only connect to one of them. Each VM has been provided a different
	// key, but the test only has the key that was written last.
	log.Println("Initialize virter")
	argv := []string{"virter", "image", "ls", "--available"}
	log.Printf("EXECUTING: %s", argv)
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
		log.Fatalf("ERROR: %v", err)
	}

	if suiteRun.vmSpec.ProvisionFile != "" {
		defer removeImages(suiteRun.vmSpec)
		if err := provisionImages(suiteRun.vmSpec, suiteRun.overrides, startVM); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				log.Print(string(exitErr.Stderr))
			}
			log.Fatal(err)
		}
	}

	nFailed, err := execTests(suiteRun, nrPool)
	if err != nil {
		log.Printf("ERROR: %v", err)
	}
	return nFailed
}

func ctxCancled(ctx context.Context) bool {
	return ctx.Err() != nil
}

func joinIfRel(basepath string, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(basepath, path)
}

func filterVMs(vms []vm, baseImages []string) []vm {
	if len(baseImages) == 0 {
		return vms
	}

	selected := []vm{}
	for _, vm := range vms {
		found := false
		for _, baseImage := range baseImages {
			if vm.BaseImage == baseImage {
				found = true
			}
		}

		if found {
			selected = append(selected, vm)
		}
	}

	return selected
}
