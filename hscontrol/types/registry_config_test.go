package types

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bearer is the whole of the head's authority on the registry channel, so
// where it may come from — and which source wins — is part of the contract
// rather than an implementation detail.
func TestResolveRegistryBearer(t *testing.T) {
	const (
		fromEnv    = "bearer-from-env-marker"    //nolint:gosec // test marker, not a real credential
		fromFile   = "bearer-from-file-marker"   //nolint:gosec // test marker, not a real credential
		fromConfig = "bearer-from-config-marker" //nolint:gosec // test marker, not a real credential
	)

	writeBearer := func(t *testing.T, contents string) string {
		t.Helper()

		path := filepath.Join(t.TempDir(), "bearer")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

		return path
	}

	t.Run("unset is empty", func(t *testing.T) {
		withViper(t, nil)

		bearer, err := resolveRegistryBearer()
		require.NoError(t, err)
		assert.Empty(t, bearer)
	})

	t.Run("the config key", func(t *testing.T) {
		withViper(t, map[string]any{"registry.bearer": fromConfig})

		bearer, err := resolveRegistryBearer()
		require.NoError(t, err)
		assert.Equal(t, fromConfig, bearer)
	})

	t.Run("a file, trimmed of the newline an editor leaves", func(t *testing.T) {
		withViper(t, map[string]any{"registry.bearer_path": writeBearer(t, fromFile+"\n")})

		bearer, err := resolveRegistryBearer()
		require.NoError(t, err)
		assert.Equal(t, fromFile, bearer)
	})

	t.Run("the environment wins over the config key", func(t *testing.T) {
		withViper(t, map[string]any{"registry.bearer": fromConfig})
		t.Setenv("REGISTRY_BEARER", fromEnv)

		bearer, err := resolveRegistryBearer()
		require.NoError(t, err)
		assert.Equal(t, fromEnv, bearer)
	})

	t.Run("the environment wins over the file, and does not read it", func(t *testing.T) {
		withViper(t, map[string]any{
			"registry.bearer_path": filepath.Join(t.TempDir(), "absent"),
		})
		t.Setenv("REGISTRY_BEARER", fromEnv)

		bearer, err := resolveRegistryBearer()
		require.NoError(t, err)
		assert.Equal(t, fromEnv, bearer)
	})

	t.Run("two config answers is a refusal", func(t *testing.T) {
		withViper(t, map[string]any{
			"registry.bearer":      fromConfig,
			"registry.bearer_path": writeBearer(t, fromFile),
		})

		_, err := resolveRegistryBearer()
		require.ErrorIs(t, err, errRegistryBearerExclusive)
	})

	// A config nobody can read is a config nobody can fix. The environment
	// winning is about which ANSWER is used; whether the file asks two
	// questions is a separate fault, and one an operator would otherwise
	// meet for the first time on the deploy that stopped injecting the
	// variable.
	t.Run("two config answers is a refusal even when the environment wins", func(t *testing.T) {
		withViper(t, map[string]any{
			"registry.bearer":      fromConfig,
			"registry.bearer_path": writeBearer(t, fromFile),
		})
		t.Setenv("REGISTRY_BEARER", fromEnv)

		_, err := resolveRegistryBearer()
		require.ErrorIs(t, err, errRegistryBearerExclusive)
	})

	t.Run("a file that is not there", func(t *testing.T) {
		withViper(t, map[string]any{
			"registry.bearer_path": filepath.Join(t.TempDir(), "absent"),
		})

		_, err := resolveRegistryBearer()
		require.Error(t, err)
	})
}

// withViper installs settings for one subtest and puts the global viper back
// afterwards, since the config loader reads it rather than being handed one.
func withViper(t *testing.T, settings map[string]any) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	for key, value := range settings {
		viper.Set(key, value)
	}
}

// The bearer is the whole of the head's authority on this channel, so a
// registry named without one is a head that would call Gentility and be
// refused every time. It refuses to boot instead: a control plane that starts
// and then serves nothing is far harder to diagnose than one that does not
// start.
func TestResolveRegistry(t *testing.T) {
	const (
		bearer = "bearer-from-config-marker" //nolint:gosec // test marker, not a real credential
		url    = "https://gentility.example"
	)

	t.Run("no registry at all", func(t *testing.T) {
		withViper(t, nil)

		gotURL, gotBearer, err := resolveRegistry()
		require.NoError(t, err)
		assert.Empty(t, gotURL)
		assert.Empty(t, gotBearer)
	})

	t.Run("a url and a bearer", func(t *testing.T) {
		withViper(t, map[string]any{"registry.url": url, "registry.bearer": bearer})

		gotURL, gotBearer, err := resolveRegistry()
		require.NoError(t, err)
		assert.Equal(t, url, gotURL)
		assert.Equal(t, bearer, gotBearer)
	})

	t.Run("a url with no bearer refuses to boot", func(t *testing.T) {
		withViper(t, map[string]any{"registry.url": url})

		_, _, err := resolveRegistry()
		require.ErrorIs(t, err, errRegistryBearerRequired)
	})

	// A bearer with no url is inert rather than wrong: nothing reads it,
	// and refusing would break a config that pre-stages the secret.
	t.Run("a bearer with no url is inert", func(t *testing.T) {
		withViper(t, map[string]any{"registry.bearer": bearer})

		gotURL, gotBearer, err := resolveRegistry()
		require.NoError(t, err)
		assert.Empty(t, gotURL)
		assert.Equal(t, bearer, gotBearer)
	})
}
