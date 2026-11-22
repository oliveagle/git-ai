package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGraftMirror_policy checks that the zero policy is off: a server that
// never configured the mirror policy must take every push, even though the
// default warn lag is not zero.
func TestGraftMirror_policy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		mirror   GraftMirror
		enabled  bool
		required bool
		warnLag  int
		denyLag  int
	}{
		{
			desc:    "the zero policy is off",
			mirror:  GraftMirror{},
			enabled: false,
		},
		{
			desc:     "a required policy warns from the default lag",
			mirror:   GraftMirror{Required: true},
			enabled:  true,
			required: true,
			warnLag:  DefaultGraftMirrorWarnLag,
		},
		{
			desc:    "a warn lag alone turns the policy on",
			mirror:  GraftMirror{WarnLag: 20},
			enabled: true,
			warnLag: 20,
		},
		{
			desc:    "a deny lag alone turns the policy on",
			mirror:  GraftMirror{DenyLag: 30},
			enabled: true,
			warnLag: DefaultGraftMirrorWarnLag,
			denyLag: 30,
		},
		{
			desc:     "a negative warn lag turns the warning off",
			mirror:   GraftMirror{Required: true, WarnLag: -1},
			enabled:  true,
			required: true,
			warnLag:  0,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			policy := tc.mirror.Policy()
			require.Equal(t, tc.enabled, policy.Enabled())
			require.Equal(t, tc.required, policy.Required)
			require.Equal(t, tc.warnLag, policy.WarnLag)
			require.Equal(t, tc.denyLag, policy.DenyLag)
		})
	}
}

func TestGraftMirror_validate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc      string
		mirror    GraftMirror
		expectErr string
	}{
		{
			desc:   "the zero policy is valid",
			mirror: GraftMirror{},
		},
		{
			desc:   "a required policy is valid",
			mirror: GraftMirror{Required: true},
		},
		{
			desc:   "a refusal after the warning is valid",
			mirror: GraftMirror{Required: true, WarnLag: 10, DenyLag: 20},
		},
		{
			desc:      "a negative deny lag is refused",
			mirror:    GraftMirror{Required: true, DenyLag: -1},
			expectErr: "deny_lag must not be negative",
		},
		{
			desc:      "a refusal without the warning it follows is refused",
			mirror:    GraftMirror{WarnLag: -1, DenyLag: 20},
			expectErr: "warn_lag is negative",
		},
		{
			desc:      "a refusal that does not follow the warning is refused",
			mirror:    GraftMirror{WarnLag: 20, DenyLag: 20},
			expectErr: "must be greater than warn_lag",
		},
		{
			desc:      "a refusal before the default warning is refused",
			mirror:    GraftMirror{DenyLag: 5},
			expectErr: "must be greater than warn_lag",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			err := tc.mirror.validate()
			if tc.expectErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.expectErr)
		})
	}
}

// TestGraftMirror_validateThroughGraft checks that a malformed policy is
// refused when the server loads its configuration, not on the first push.
func TestGraftMirror_validateThroughGraft(t *testing.T) {
	t.Parallel()

	err := Graft{Mirror: GraftMirror{WarnLag: 20, DenyLag: 10}}.Validate()
	require.ErrorContains(t, err, "graft mirror: deny_lag")
}
