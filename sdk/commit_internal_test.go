package sdk

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// unknownAction is an action the SDK does not know. Only the package itself
// can hold one, because the interface it implements is not exported.
type unknownAction struct{}

func (unknownAction) isAction() {}

// TestTheUnknownActionIsRefused covers the action the SDK cannot send, in the
// validation of a commit and in the sending of it.
func TestTheUnknownActionIsRefused(t *testing.T) {
	err := validateAction(unknownAction{})
	require.Equal(t, codes.InvalidArgument, Code(err))
	require.Contains(t, err.Error(), "unsupported action type")

	err = sendAction(nil, unknownAction{})
	require.Equal(t, codes.InvalidArgument, Code(err))
	require.Contains(t, err.Error(), "unsupported action type")

	err = validateCommitOptions(CommitOptions{
		Branch:  "main",
		Message: "change",
		Actions: []Action{unknownAction{}},
	})
	require.Equal(t, codes.InvalidArgument, Code(err))
}
