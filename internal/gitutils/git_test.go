package gitutils

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyResolvedRepo(t *testing.T) {
	info := &RepoInfo{Branch: "feature/mlb-clock-reliability", CommitHash: "bbfcff4e02aae5ea50e60a961a5456e7af329da5"}
	require.NoError(t, verifyResolvedRepo(info, "feature/mlb-clock-reliability", "bbfcff4e0"))
	require.ErrorContains(t, verifyResolvedRepo(info, "main", "bbfcff4e0"), "expected \"main\"")
	require.ErrorContains(t, verifyResolvedRepo(info, info.Branch, "4b284f197"), "expected 4b284f197")
}
