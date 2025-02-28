package cmd

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
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
	ProvisionBootCap string   `toml:"provision_boot_capacity"`
	ProvisionMemory  string   `toml:"provision_memory"`
	ProvisionCPUs    uint     `toml:"provision_cpus"`
	VMs              []vm     `toml:"vms"`
}

func (s *vmSpecification) ImageName(v *vm) string {
	if s.ProvisionFile == "" {
		// No provisioning, use base image directly
		return v.ID()
	}
	return fmt.Sprintf("%s-%s", v.ID(), s.Name)
}

type testSpecification struct {
	TestSuiteFile string          `toml:"test_suite_file"`
	TestTimeout   duration        `toml:"test_timeout"`
	Tests         map[string]test `toml:"tests"`
	Networks      []virterNet     `toml:"networks"` // Extra NIC to add to the VMs for all tests
	Artifacts     []string        `toml:"artifacts"`
	Variants      []variant       `toml:"variants"`
}

type variant struct {
	Name      string            `toml:"name"`
	Variables map[string]string `toml:"variables"`
	IPv6      bool              `toml:"ipv6"`
	VMTags    []string          `toml:"vm_tags"`
}

type virterNet struct {
	ForwardMode string `toml:"forward"`
	IPv6        bool   `toml:"ipv6"`
	DHCP        bool   `toml:"dhcp"`
	Domain      string `toml:"domain"`
}

type FailurePolicy string

const (
	OnFailureContinue  FailurePolicy = "continue"
	OnFailureTerminate FailurePolicy = "terminate"
	OnFailureKeepVms   FailurePolicy = "keep-vms"
)

type testSuiteRun struct {
	vmSpec            *vmSpecification
	testSpec          *testSpecification
	overrides         []string
	outDir            string
	testRuns          []testRun
	startVM           int
	nrVMs             int
	firstV4Net        *net.IPNet
	firstV6Net        *net.IPNet
	onFailure         FailurePolicy
	printErrorDetails bool
	logFormatVirter   string
	pullImageTemplate *template.Template
}

func (f *FailurePolicy) String() string {
	return string(*f)
}

func (f *FailurePolicy) Set(v string) error {
	switch v {
	case "continue", "terminate", "keep-vms":
		*f = FailurePolicy(v)
		return nil
	default:
		return errors.New("should be one out of \"continue\" \"terminate\" \"keep-vms\"")
	}
}

func (f *FailurePolicy) Type() string {
	return "FailurePolicy"
}

type testConfig struct {
	testLogDir string
	vmSpec     *vmSpecification
	testName   string
	test       test
	repeats    int
	networks   []virterNet // includes networks configured for all tests as well as for this test specifically
}

type TemplateFlag struct {
	*template.Template
}

func (t *TemplateFlag) String() string {
	if t.Template != nil {
		return t.Template.Tree.Root.String()
	} else {
		return ""
	}
}

func (t *TemplateFlag) Set(s string) error {
	parsed, err := template.New("image").Parse(s)
	if err != nil {
		return err
	}

	t.Template = parsed
	return nil
}

func (t *TemplateFlag) Type() string {
	return "template"
}

// Execute runs vmshed
func Execute() {
	log.SetFormatter(VmshedStandardLogFormatter())

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
	var excludeBaseImages []string
	var toRun string
	var repeats int
	var startVM int
	var nrVMs int
	var onFailure FailurePolicy = OnFailureContinue
	var quiet bool
	var logFormatVirter string
	var outDir string
	var version bool
	var variantsToRun []string
	var errorDetails bool
	var firstv4Subnet string
	var firstv6Subnet string
	var pullImageTemplate TemplateFlag

	rootCmd := &cobra.Command{
		Use:   "vmshed",
		Short: "Run tests in VMs",
		Long: `Run tests in VMs.

Logs and results in XML and JSON formats are written to an
output directory. It must be possible for libvirt to write to
files in this directory. NFS mounts generally cannot be used
due to root_squash. Nonetheless, the files will be owned by the
current user.`,
		Run: func(cmd *cobra.Command, args []string) {
			if version {
				fmt.Println(prog, config.Version)
				return
			}

			if quiet {
				log.SetLevel(log.InfoLevel)
			} else {
				log.SetLevel(log.DebugLevel)
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

			log.Infof("Using random seed: %d", randomSeed)
			randomGenerator := rand.New(rand.NewSource(randomSeed))

			err := os.MkdirAll(outDir, 0755)
			if err != nil {
				log.Fatalf("could not mkdir %s: %v", outDir, err)
			}

			var vmSpec vmSpecification
			if _, err := toml.DecodeFile(vmSpecPath, &vmSpec); err != nil {
				log.Fatal(err)
			}
			vmSpec.ProvisionFile = joinIfRel(filepath.Dir(vmSpecPath), vmSpec.ProvisionFile)
			vmSpec.ProvisionTimeout = durationDefault(vmSpec.ProvisionTimeout, 3*time.Minute)
			vmSpec.VMs = filterVMs(vmSpec.VMs, baseImages, excludeBaseImages)

			var testSpec testSpecification
			if _, err := toml.DecodeFile(testSpecPath, &testSpec); err != nil {
				log.Fatal(err)
			}
			if testSpec.TestSuiteFile == "" {
				testSpec.TestSuiteFile = "run.toml"
			}
			testSpec.TestSuiteFile = joinIfRel(filepath.Dir(testSpecPath), testSpec.TestSuiteFile)
			testSpec.TestTimeout = durationDefault(testSpec.TestTimeout, 5*time.Minute)

			suiteRun, err := createTestSuiteRun(randomGenerator, vmSpec, testSpec, toRun, outDir, repeats, variantsToRun)
			if err != nil {
				log.Fatal(err)
			}

			_, firstV4Net, err := net.ParseCIDR(firstv4Subnet)
			if err != nil {
				log.Fatal(err)
			}

			_, firstV6Net, err := net.ParseCIDR(firstv6Subnet)
			if err != nil {
				log.Fatal(err)
			}

			suiteRun.overrides = provisionOverrides
			suiteRun.startVM = startVM
			suiteRun.nrVMs = nrVMs
			suiteRun.onFailure = onFailure
			suiteRun.printErrorDetails = errorDetails
			suiteRun.firstV4Net = firstV4Net
			suiteRun.firstV6Net = firstV6Net
			suiteRun.logFormatVirter = logFormatVirter
			suiteRun.pullImageTemplate = pullImageTemplate.Template

			ctx, cancel := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
			defer cancel()
			start := time.Now()

			results, err := provisionAndExec(ctx, &suiteRun)
			if err != nil {
				log.Errorf("ERROR: %v", err)
				unwrapStderr(err)
			}

			if err := saveResultsJSON(suiteRun, start, results); err != nil {
				log.Warnf("Failed to save JSON results: %v", err)
			}

			exitCode := printSummaryTable(suiteRun, results)

			log.Infoln("OVERALL EXECUTIONTIME:", time.Since(start).Round(time.Second))
			os.Exit(exitCode)
		},
	}

	rootCmd.Flags().StringVarP(&vmSpecPath, "vms", "", "vms.toml", "File containing VM specification")
	rootCmd.Flags().StringVarP(&testSpecPath, "tests", "", "tests.toml", "File containing test specification")
	rootCmd.Flags().StringArrayVarP(&provisionOverrides, "set", "s", []string{}, "set/override provisioning steps, for example '--set values.X=y'")
	rootCmd.Flags().StringSliceVarP(&baseImages, "base-image", "", []string{}, "VM base images to use (defaults to all)")
	rootCmd.Flags().StringSliceVarP(&excludeBaseImages, "exclude-base-image", "", []string{}, "VM base images to exclude (defaults to none)")
	rootCmd.Flags().StringVarP(&toRun, "torun", "", "all", "comma separated list of test names to execute ('all' is a reserved test name)")
	rootCmd.Flags().IntVarP(&repeats, "repeats", "", 1, "number of times to repeat each test, expecting success on every attempt")
	rootCmd.Flags().IntVarP(&startVM, "startvm", "", 2, "Number of the first VM to start in parallel")
	rootCmd.Flags().IntVarP(&nrVMs, "nvms", "", 12, "Maximum number of VMs to start in parallel, starting at -startvm")
	rootCmd.Flags().VarP(&onFailure, "on-failure", "", "What to do if a test fails: continue|terminate|keep-vms")
	rootCmd.Flags().BoolVarP(&quiet, "quiet", "", false, "Don't print progess messages while tests are running")
	rootCmd.Flags().StringVar(&logFormatVirter, "virter-log-format", "", "Log format that is passed to virter on vm exec")
	rootCmd.Flags().StringVarP(&outDir, "out-dir", "", "tests-out", "Directory for test results and logs")
	rootCmd.Flags().BoolVarP(&version, "version", "", false, "Print version and exit")
	rootCmd.Flags().Int64VarP(&randomSeed, "seed", "", 0, "The random number generator seed to use. Specifying 0 seeds with the current time (the default)")
	rootCmd.Flags().StringSliceVarP(&variantsToRun, "variant", "", []string{}, "which variant to run (defaults to all)")
	rootCmd.Flags().BoolVarP(&errorDetails, "error-details", "", true, "Show all test error logs at the end of the run")
	rootCmd.Flags().StringVarP(&firstv4Subnet, "first-subnet", "", "10.224.0.0/24", "The first subnet to use for VMs. If more virtual networks are required, the next higher network of the same size will be used")
	rootCmd.Flags().StringVarP(&firstv6Subnet, "first-v6-subnet", "", "fd62:a80c:412::/64", "The first ipv6 subnet to use for VMs. If more virtual networks are required, the next higher network of the same size will be used")
	rootCmd.Flags().VarP(&pullImageTemplate, "pull-template", "", "Where to pull the base images from. Accepts a go template string, allowing usage like 'registry.example.com/vm/{{ .Image }}:latest'")
	return rootCmd
}

func createTestSuiteRun(
	randomGenerator *rand.Rand,
	vmSpec vmSpecification,
	testSpec testSpecification,
	toRun string,
	outDir string,
	repeats int,
	variantsToRun []string) (testSuiteRun, error) {

	for testName := range testSpec.Tests {
		if toRun != "all" && toRun != "" { //filter tests to Run
			if !containsString(strings.Split(toRun, ","), testName) {
				delete(testSpec.Tests, testName)
			}
		}
	}

	testSpec.Variants = filterVariants(testSpec.Variants, variantsToRun)

	testLogDir := filepath.Join(outDir, "log")
	testRuns, err := determineAllTestRuns(randomGenerator, testLogDir, &vmSpec, &testSpec, repeats)
	if err != nil {
		log.Fatal(err)
	}
	vmSpec.VMs = removeUnusedVMs(vmSpec.VMs, testRuns)

	for _, run := range testRuns {
		images := make([]string, len(run.vms))
		for i, v := range run.vms {
			images[i] = v.ID()
		}
		imageString := strings.Join(images, ",")
		log.Infof("PLAN: %s on %s", run.testID, imageString)
	}

	suiteRun := testSuiteRun{
		vmSpec:   &vmSpec,
		testSpec: &testSpec,
		outDir:   outDir,
		testRuns: testRuns,
	}

	return suiteRun, nil
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
	log.Infoln("|===================================================================================================")
	log.Infof("| ** Results: %d/%d successful (%.2f%%)", success, len(suiteRun.testRuns), successRate)
	log.Infoln("|===================================================================================================")
	sortedTestRuns := suiteRun.testRuns
	sort.SliceStable(sortedTestRuns, func(i, j int) bool {
		return sortedTestRuns[i].testName < sortedTestRuns[j].testName
	})
	for _, testRun := range sortedTestRuns {
		status := StatusSkipped
		tduration := 0 * time.Second
		if result, ok := results[testRun.testID]; ok {
			status = result.status
			tduration = result.execTime
		}
		log.Infof("| %-11s: %-73s : %9s", status, testRun.testID, tduration.Round(time.Second))
	}
	log.Infoln("|===================================================================================================")
	logViewer := getLogViewUrl("")
	if logViewer != "" {
		log.Infof("| ** Logs: %s", logViewer)
		log.Infoln("|===================================================================================================")
	}

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
	randomGenerator *rand.Rand,
	testLogDir string,
	vmSpec *vmSpecification,
	testSpec *testSpecification,
	repeats int) ([]testRun, error) {

	testRuns := []testRun{}
	for testName, test := range testSpec.Tests {
		config := testConfig{
			testLogDir: testLogDir,
			vmSpec:     vmSpec,
			testName:   testName,
			test:       test,
			repeats:    repeats,
			networks:   append(testSpec.Networks, test.Networks...),
		}
		runs, err := determineRunsForTest(randomGenerator, &config, testSpec.Variants)
		if err != nil {
			return nil, err
		}
		testRuns = append(testRuns, runs...)
	}
	return testRuns, nil
}

func determineRunsForTest(randomGenerator *rand.Rand, config *testConfig, variants []variant) ([]testRun, error) {
	testRuns := []testRun{}

	for _, variant := range variants {
		// only add variants that are selected
		if len(config.test.Variants) > 0 && !containsString(config.test.Variants, variant.Name) {
			continue
		}

		variantVMs := matchingVMTags(variant.VMTags, config.vmSpec.VMs)
		availableVMs := matchingVMTags(config.test.VMTags, variantVMs)
		if len(availableVMs) == 0 {
			log.Infof("SKIP: test:%s variant:%s - no available VMs", config.testName, variant.Name)
			continue
		}

		for _, vmCount := range config.test.VMCount {
			variantRuns, err := determineRunsForTestVariant(randomGenerator, config, vmCount, variant, availableVMs)
			if err != nil {
				return []testRun{}, err
			}
			testRuns = append(testRuns, variantRuns...)
		}
	}

	return testRuns, nil
}

func determineRunsForTestVariant(randomGenerator *rand.Rand, config *testConfig, vmCount int, testVariant variant, availableVMs []vm) ([]testRun, error) {
	testRuns := []testRun{}

	for repeatCounter := 0; repeatCounter < config.repeats; repeatCounter++ {
		if config.test.NeedAllPlatforms {
			for _, v := range availableVMs {
				testRuns = append(testRuns, newTestRun(
					config, testVariant, repeatVM(v, vmCount), len(testRuns)))
			}
		} else if config.test.SameVMs {
			v, err := randomVM(randomGenerator, availableVMs)
			if err != nil {
				return nil, err
			}
			testRuns = append(testRuns, newTestRun(
				config, testVariant, repeatVM(v, vmCount), len(testRuns)))
		} else {
			var vms []vm
			for i := 0; i < vmCount; i++ {
				v, err := randomVM(randomGenerator, availableVMs)
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

func randomVM(randomGenerator *rand.Rand, vms []vm) (vm, error) {
	if len(vms) == 0 {
		return vm{}, fmt.Errorf("Unable to randomly choose VM from empty array")
	}
	return vms[randomGenerator.Int63n(int64(len(vms)))], nil
}

func containsString(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func containsAllStrings(values, required []string) bool {
	for _, e := range required {
		if !containsString(values, e) {
			return false
		}
	}
	return true
}

func matchingVMTags(requiredVMTags []string, vms []vm) []vm {
	possibleVMs := []vm{}
	for _, vm := range vms {
		if containsAllStrings(vm.VMTags, requiredVMTags) {
			possibleVMs = append(possibleVMs, vm)
		}
	}
	return possibleVMs
}

func newTestRun(config *testConfig, variant variant, vms []vm, testIndex int) testRun {
	testID := testIDString(config.testName, len(vms), variant.Name, testIndex)

	run := testRun{
		testName: config.testName,
		testID:   testID,
		outDir:   filepath.Join(config.testLogDir, testID),
		vms:      vms,
		networks: config.networks,
		variant:  variant,
	}

	return run
}

func provisionAndExec(ctx context.Context, suiteRun *testSuiteRun) (map[string]testResult, error) {
	// Note: When virter first starts it generates a key pair. However,
	// when we start multiple instances concurrently, they race. The result
	// is that the VMs start successfully, but then the test can only
	// connect to one of them. Each VM has been provided a different key,
	// but the test only has the key that was written last. Hence the first
	// virter command run should not be parallel.
	argv := []string{"virter", "image", "ls", "--available"}
	log.Debugf("EXECUTING: %s", argv)
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil {
		return map[string]testResult{}, fmt.Errorf("cannot initialize virter: %w", err)
	}

	defer removeImages(suiteRun.outDir, suiteRun.vmSpec)

	results := runScheduler(ctx, suiteRun)
	return results, nil
}

func joinIfRel(basepath string, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(basepath, path)
}

// filterVMs filters the VMs based on the baseImages and exclude parameters.
// If baseImages is empty, all VMs are included. If exclude is empty, no VMs are excluded.
func filterVMs(vms []vm, baseImages []string, exclude []string) []vm {
	if len(baseImages) == 0 && len(exclude) == 0 {
		return vms
	}

	selected := make([]vm, 0)
	for _, vm := range vms {
		found := false
		for _, baseImage := range baseImages {
			if vm.BaseImage == baseImage {
				found = true
			}
		}

		if len(baseImages) == 0 {
			// default for empty baseImages is to include all
			found = true
		}

		for _, excludeImage := range exclude {
			if vm.BaseImage == excludeImage {
				found = false
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
			usedVMs[v.ID()] = true
		}
	}

	selected := []vm{}
	for _, v := range vms {
		if usedVMs[v.ID()] {
			selected = append(selected, v)
		}
	}

	return selected
}
