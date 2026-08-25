package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testProvisionToml = `version = 1

[values]
A = "a"
B = "b"
X = "x"
Y = "y"
"X=x" = "quoted"

[[steps]]
`

func TestProvisionBuildID(t *testing.T) {
	provisionPath := filepath.Join(t.TempDir(), "provision.toml")
	require.NoError(t, os.WriteFile(provisionPath, []byte(testProvisionToml), 0644))

	makeSuiteRun := func() *testSuiteRun {
		return &testSuiteRun{
			vmSpec: &vmSpecification{
				ProvisionFile:    provisionPath,
				ProvisionBootCap: "10G",
			},
			overrides: []string{"values.A=1", "values.B=2"},
		}
	}
	makeVM := func() *vm {
		return &vm{
			Values:   map[string]string{"X": "x", "Y": "y"},
			UserName: "root",
		}
	}

	original, err := provisionBuildID(makeSuiteRun(), makeVM())
	require.NoError(t, err)

	repeated, err := provisionBuildID(makeSuiteRun(), makeVM())
	require.NoError(t, err)
	assert.Equal(t, original, repeated, "build ID should be deterministic")

	t.Run("provision file content", func(t *testing.T) {
		changedPath := filepath.Join(t.TempDir(), "provision.toml")
		require.NoError(t, os.WriteFile(changedPath, []byte(testProvisionToml+"[steps.shell]\n"), 0644))
		suiteRun := makeSuiteRun()
		suiteRun.vmSpec.ProvisionFile = changedPath
		changed, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})

	t.Run("override content", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.overrides = []string{"values.A=1", "values.B=3"}
		changed, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})

	t.Run("override order", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.overrides = []string{"values.B=2", "values.A=1"}
		changed, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.NotEqual(t, original, changed, "later overrides take precedence, so order matters")
	})

	t.Run("undeclared override ignored", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.overrides = append(suiteRun.overrides, "values.TestOnly=1")
		unchanged, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.Equal(t, original, unchanged,
			"overrides of values not declared in the provision file cannot affect provisioning")
	})

	t.Run("undeclared multi-assignment override ignored", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.overrides = append(suiteRun.overrides, "values.TestOnly=1,values.OtherTestOnly=2")
		unchanged, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.Equal(t, original, unchanged)
	})

	t.Run("mixed multi-assignment override hashed", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.overrides = append(suiteRun.overrides, "values.TestOnly=1,values.A=2")
		changed, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})

	t.Run("non-values override hashed", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.overrides = append(suiteRun.overrides, "env.FOO=bar")
		changed, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})

	t.Run("vm values", func(t *testing.T) {
		v := makeVM()
		v.Values["X"] = "z"
		changed, err := provisionBuildID(makeSuiteRun(), v)
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})

	t.Run("undeclared vm value ignored", func(t *testing.T) {
		v := makeVM()
		v.Values["TestOnly"] = "1"
		unchanged, err := provisionBuildID(makeSuiteRun(), v)
		require.NoError(t, err)
		assert.Equal(t, original, unchanged)
	})

	t.Run("vm value key/value boundary", func(t *testing.T) {
		a, err := provisionBuildID(makeSuiteRun(), &vm{Values: map[string]string{"X": "x=1"}, UserName: "root"})
		require.NoError(t, err)
		b, err := provisionBuildID(makeSuiteRun(), &vm{Values: map[string]string{"X=x": "1"}, UserName: "root"})
		require.NoError(t, err)
		assert.NotEqual(t, a, b)
	})

	t.Run("boot capacity", func(t *testing.T) {
		suiteRun := makeSuiteRun()
		suiteRun.vmSpec.ProvisionBootCap = "20G"
		changed, err := provisionBuildID(suiteRun, makeVM())
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})

	t.Run("user name", func(t *testing.T) {
		v := makeVM()
		v.UserName = "Administrator"
		changed, err := provisionBuildID(makeSuiteRun(), v)
		require.NoError(t, err)
		assert.NotEqual(t, original, changed)
	})
}
