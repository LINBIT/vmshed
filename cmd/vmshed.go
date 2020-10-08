package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/sethvargo/go-signalcontext"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/LINBIT/vmshed/cmd/config"
)

type duration time.Duration

func (d *duration) UnmarshalText(text []byte) error {
	timeDuration, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = duration(timeDuration)
	return nil
}

func durationDefault(d duration, fallback time.Duration) duration {
	if d == 0 {
		return duration(fallback)
	}
	return d
}

type vmSpecification struct {
	Name             string   `toml:"name"`
	ProvisionFile    string   `toml:"provision_file"`
	ProvisionTimeout duration `toml:"provision_timeout"`
	VMs              []vm     `toml:"vms"`
}

func (s *vmSpecification) ImageName(v *vm) string {
	if s.ProvisionFile == "" {
		// No provisioning, use base image directly
		return v.BaseImage
	}
	return fmt.Sprintf("%s-%s", v.BaseImage, s.Name)
}

type testSpecification struct {
	TestSuiteFile string          `toml:"test_suite_file"`
	TestTimeout   duration        `toml:"test_timeout"`
	Tests         map[string]test `toml:"tests"`
	Artifacts     []string        `toml:"artifacts"`
	Variants      []variant       `toml:"variants"`
}

type variant struct {
	Name      string            `toml:"name"`
	Variables map[string]string `toml:"variables"`
}

type testSuiteRun struct {
	vmSpec    *vmSpecification
	testSpec  *testSpecification
	overrides []string
	jenkins   *Jenkins
	testRuns  []testRun
	startVM   int
	nrVMs     int
	failTest  bool
	quiet     bool
}

type testConfig struct {
	jenkins  *Jenkins
	vmSpec   *vmSpecification
	testName string
	test     test
	repeats  int
}

// Execute runs vmshed
func Execute() {
	log.SetFormatter(&log.TextFormatter{
		TimestampFormat: "2006-01-02 15:04:05.000",
	})

	if err := rootCommand().Execute(); err != nil {
		log.Fatal(err)
	}
}

func rootCommand() *cobra.Command {
	prog := path.Base(os.Args[0])

	var vmSpecPath string
	var testSpecPath string
	var randomSeed int64
	var provisionOverrides []string
	var baseImages []string
	var toRun string
	var repeats int
	var startVM int
	var nrVMs int
	var failTest bool
	var quiet bool
	var jenkinsWS string
	var version bool
	var variantsToRun []string

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
				log.Fatal("--startvm has to be positive")
			}
			if nrVMs <= 0 {
				log.Fatal("--nvms has to be positive")
			}

			if randomSeed == 0 {
				randomSeed = time.Now().UTC().UnixNano()
			}

			log.Printf("Using random seed: %d", randomSeed)
			rand.Seed(randomSeed)

			var vmSpec vmSpecification
			if _, err := toml.DecodeFile(vmSpecPath, &vmSpec); err != nil {
				log.Fatal(err)
			}
			vmSpec.ProvisionFile = joinIfRel(filepath.Dir(vmSpecPath), vmSpec.ProvisionFile)
			vmSpec.ProvisionTimeout = durationDefault(vmSpec.ProvisionTimeout, 3*time.Minute)
			vmSpec.VMs = filterVMs(vmSpec.VMs, baseImages)

			var testSpec testSpecification
			if _, err := toml.DecodeFile(testSpecPath, &testSpec); err != nil {
				log.Fatal(err)
			}
			if testSpec.TestSuiteFile == "" {
				testSpec.TestSuiteFile = "run.toml"
			}
			testSpec.TestSuiteFile = joinIfRel(filepath.Dir(testSpecPath), testSpec.TestSuiteFile)
			testSpec.TestTimeout = durationDefault(testSpec.TestTimeout, 5*time.Minute)

			suiteRun, err := createTestSuiteRun(vmSpec, testSpec, baseImages, toRun, jenkinsWS, repeats, variantsToRun)
			if err != nil {
				log.Fatal(err)
			}

			suiteRun.overrides = provisionOverrides
			suiteRun.startVM = startVM
			suiteRun.nrVMs = nrVMs
			suiteRun.failTest = failTest
			suiteRun.quiet = quiet

			ctx, cancel := signalcontext.On(unix.SIGINT, unix.SIGTERM)
			defer cancel()
			start := time.Now()

			results, err := provisionAndExec(ctx, filepath.Base(os.Args[0]), &suiteRun)
			if err != nil {
				log.Errorf("ERROR: %v", err)
				unwrapStderr(err)
			}

			if suiteRun.jenkins != nil {
				err := saveResultsJSON(suiteRun.jenkins, suiteRun, results)
				if err != nil {
					log.Warnf("Failed to save JSON results: %v", err)
				}
			}

			exitCode := printSummaryTable(suiteRun, results)

			log.Println("OVERALL EXECUTIONTIME:", time.Since(start))
			os.Exit(exitCode)
		},
	}

	rootCmd.Flags().StringVarP(&vmSpecPath, "vms", "", "vms.toml", "File containing VM specification")
	rootCmd.Flags().StringVarP(&testSpecPath, "tests", "", "tests.toml", "File containing test specification")
	rootCmd.Flags().StringSliceVarP(&provisionOverrides, "set", "s", []string{}, "set/override provisioning steps, for example '--set values.X=y'")
	rootCmd.Flags().StringSliceVarP(&baseImages, "base-image", "", []string{}, "VM base images to use (defaults to all)")
	rootCmd.Flags().StringVarP(&toRun, "torun", "", "all", "comma separated list of test names to execute ('all' is a reserved test name)")
	rootCmd.Flags().IntVarP(&repeats, "repeats", "", 1, "number of times to repeat each test, expecting success on every attempt")
	rootCmd.Flags().IntVarP(&startVM, "startvm", "", 2, "Number of the first VM to start in parallel")
	rootCmd.Flags().IntVarP(&nrVMs, "nvms", "", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	rootCmd.Flags().BoolVarP(&failTest, "failtest", "", false, "Stop executing tests when the first one failed")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "", false, "Don't print progess messages while tests are running")
	rootCmd.Flags().StringVarP(&jenkinsWS, "jenkins", "", "", "If this is set to a path for the current job, text output is saved to files, logs get copied,...")
	rootCmd.Flags().BoolVarP(&version, "version", "", false, "Print version and exit")
	rootCmd.Flags().Int64VarP(&randomSeed, "seed", "", 0, "The random number generator seed to use. Specifying 0 seeds with the current time (the default)")
	rootCmd.Flags().StringSliceVarP(&variantsToRun, "variant", "", []string{}, "which variant to run (defaults to all)")

	return rootCmd
}

func createTestSuiteRun(
	vmSpec vmSpecification,
	testSpec testSpecification,
	baseImages []string,
	toRun string,
	jenkinsWS string,
	repeats int,
	variantsToRun []string) (testSuiteRun, error) {

	jenkins, err := NewJenkins(jenkinsWS)
	if err != nil {
		log.Fatal(err)
	}

	for testName := range testSpec.Tests {
		if toRun != "all" && toRun != "" { //filter tests to Run
			if !containsString(strings.Split(toRun, ","), testName) {
				delete(testSpec.Tests, testName)
			}
		}
	}

	testSpec.Variants = filterVariants(testSpec.Variants, variantsToRun)

	testRuns, err := determineAllTestRuns(jenkins, &vmSpec, &testSpec, repeats)
	if err != nil {
		log.Fatal(err)
	}
	vmSpec.VMs = removeUnusedVMs(vmSpec.VMs, testRuns)

	for _, run := range testRuns {
		baseImages := make([]string, len(run.vms))
		for i, v := range run.vms {
			baseImages[i] = v.BaseImage
		}
		baseImageString := strings.Join(baseImages, ",")
		log.Printf("PLAN: %s on %s", run.testID, baseImageString)
	}

	suiteRun := testSuiteRun{
		vmSpec:   &vmSpec,
		testSpec: &testSpec,
		jenkins:  jenkins,
		testRuns: testRuns,
	}

	return suiteRun, nil
}

func saveResultsJSON(jenkins *Jenkins, suiteRun testSuiteRun, results map[string]testResult) error {
	type resultData struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}

	filename := jenkins.SubDir("results.json")
	log.Printf("Saving results as JSON to %s", filename)
	dest, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("Failed to create results JSON file: %w", err)
	}

	var allResults []resultData
	for _, testRun := range suiteRun.testRuns {
		resultString := "SKIPPED"
		if result, ok := results[testRun.testID]; ok {
			resultString = result.stateString
		}

		allResults = append(allResults, resultData{
			ID:    testRun.testID,
			State: resultString,
		})

	}
	err = json.NewEncoder(dest).Encode(&allResults)
	if err != nil {
		return fmt.Errorf("failed to encode results JSON: %w", err)
	}

	return nil
}

func printSummaryTable(suiteRun testSuiteRun, results map[string]testResult) int {
	exitCode := 0
	success := 0
	// count successes
	for _, testRun := range suiteRun.testRuns {
		if result, ok := results[testRun.testID]; ok && result.err == nil {
			success++
		} else {
			exitCode = 1
		}
	}
	successRate := (float32(success) / float32(len(suiteRun.testRuns))) * 100
	log.Println("|===================================================================================================")
	log.Printf("| ** Results: %d/%d successful (%.2f%%)", success, len(suiteRun.testRuns), successRate)
	log.Println("|===================================================================================================")
	for _, testRun := range suiteRun.testRuns {
		resultString := "SKIPPED"
		if result, ok := results[testRun.testID]; ok {
			resultString = result.stateString
		}
		log.Printf("| %-20s: %s", resultString, testRun.testID)
	}
	log.Println("|===================================================================================================")
	return exitCode
}

func filterVariants(variants []variant, variantsToRun []string) []variant {
	if len(variants) == 0 {
		// just the default variant
		return []variant{{Name: "default"}}
	}

	if len(variantsToRun) == 0 {
		return variants
	}

	var selectedVariants []variant
	for _, variant := range variants {
		if containsString(variantsToRun, variant.Name) {
			selectedVariants = append(selectedVariants, variant)
		}
	}
	return selectedVariants
}

func determineAllTestRuns(
	jenkins *Jenkins,
	vmSpec *vmSpecification,
	testSpec *testSpecification,
	repeats int) ([]testRun, error) {

	testRuns := []testRun{}
	for testName, test := range testSpec.Tests {
		config := testConfig{
			jenkins:  jenkins,
			vmSpec:   vmSpec,
			testName: testName,
			test:     test,
			repeats:  repeats,
		}
		runs, err := determineRunsForTest(&config, testSpec.Variants)
		if err != nil {
			return nil, err
		}
		testRuns = append(testRuns, runs...)
	}
	return testRuns, nil
}

func determineRunsForTest(config *testConfig, variants []variant) ([]testRun, error) {
	testRuns := []testRun{}

	for _, variant := range variants {
		// only add variants that are selected
		if len(config.test.Variants) > 0 && !containsString(config.test.Variants, variant.Name) {
			continue
		}

		for _, vmCount := range config.test.VMCount {
			variantRuns, err := determineRunsForTestVariant(config, vmCount, variant)
			if err != nil {
				return []testRun{}, err
			}
			testRuns = append(testRuns, variantRuns...)
		}
	}

	return testRuns, nil
}

func determineRunsForTestVariant(config *testConfig, vmCount int, testVariant variant) ([]testRun, error) {
	testRuns := []testRun{}

	for repeatCounter := 0; repeatCounter < config.repeats; repeatCounter++ {
		if config.test.NeedAllPlatforms {
			for _, v := range matchingVMTags(config) {
				testRuns = append(testRuns, newTestRun(
					config, testVariant, repeatVM(v, vmCount), len(testRuns)))
			}
		} else if config.test.SameVMs {
			v, err := randomVMWithTags(config)
			if err != nil {
				return nil, err
			}
			testRuns = append(testRuns, newTestRun(
				config, testVariant, repeatVM(v, vmCount), len(testRuns)))
		} else {
			var vms []vm
			for i := 0; i < vmCount; i++ {
				v, err := randomVMWithTags(config)
				if err != nil {
					return nil, err
				}
				vms = append(vms, v)
			}
			testRuns = append(testRuns, newTestRun(config, testVariant, vms, len(testRuns)))
		}
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
	if len(vms) == 0 {
		return vm{}, fmt.Errorf("Unable to random VM from empty array")
	}
	return vms[rand.Int63n(int64(len(vms)))], nil
}

func containsString(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func matchingVMTags(config *testConfig) []vm {
	possibleVMs := []vm{}
	for _, vm := range config.vmSpec.VMs {
		hasAllTags := true
		for _, tag := range config.test.Tags {
			if !containsString(vm.Tags, tag) {
				hasAllTags = false
				break
			}
		}
		if hasAllTags {
			possibleVMs = append(possibleVMs, vm)
		}
	}
	return possibleVMs
}

func randomVMWithTags(config *testConfig) (vm, error) {
	return randomVM(matchingVMTags(config))
}

func newTestRun(config *testConfig, variant variant, vms []vm, testIndex int) testRun {
	run := testRun{
		testName: config.testName,
		testID:   testIDString(config.testName, len(vms), variant.Name, testIndex),
		vms:      vms,
		variant:  variant,
	}

	if config.jenkins.IsActive() {
		run.outDir = config.jenkins.SubDir(filepath.Join("log", run.testID))
	}

	return run
}

func provisionAndExec(ctx context.Context, cmdName string, suiteRun *testSuiteRun) (map[string]testResult, error) {
	// Note: When virter first starts it generates a key pair. However,
	// when we start multiple instances concurrently, they race. The result
	// is that the VMs start successfully, but then the test can only
	// connect to one of them. Each VM has been provided a different key,
	// but the test only has the key that was written last. Hence the first
	// virter command run should not be parallel.
	err := addNetworkHosts(ctx, suiteRun)
	if err != nil {
		return nil, err
	}

	defer removeNetworkHosts(suiteRun)

	defer removeImages(suiteRun.vmSpec)

	log.Println("STAGE: Scheduler")
	results := runScheduler(ctx, suiteRun)
	return results, nil
}

func addNetworkHosts(ctx context.Context, suiteRun *testSuiteRun) error {
	log.Println("STAGE: Add network host mappings")
	argv := []string{
		"virter", "network", "host", "add",
		"--id", strconv.Itoa(suiteRun.startVM),
		"--count", strconv.Itoa(suiteRun.nrVMs)}
	log.Printf("EXECUTING: %s", argv)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()
	if err := cmdStderrTerm(ctx, log.StandardLogger(), cmd); err != nil {
		return fmt.Errorf("cannot add network host mappings: %w", err)
	}
	return nil
}

func removeNetworkHosts(suiteRun *testSuiteRun) {
	log.Println("STAGE: Remove network host mappings")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	argv := []string{
		"virter", "network", "host", "rm",
		"--id", strconv.Itoa(suiteRun.startVM),
		"--count", strconv.Itoa(suiteRun.nrVMs)}
	log.Printf("EXECUTING: %s", argv)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = virterEnv()
	if err := cmdStderrTerm(ctx, log.StandardLogger(), cmd); err != nil {
		log.Errorf("CLEANUP: cannot remove network host mappings: %v", err)
		dumpStderr(log.StandardLogger(), err)
	}
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

func removeUnusedVMs(vms []vm, testRuns []testRun) []vm {
	usedVMs := make(map[string]bool)

	for _, run := range testRuns {
		for _, v := range run.vms {
			usedVMs[v.BaseImage] = true
		}
	}

	selected := []vm{}
	for _, v := range vms {
		if usedVMs[v.BaseImage] {
			selected = append(selected, v)
		}
	}

	return selected
}
