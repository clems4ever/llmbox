package guest

// maxExecOutput caps a single Exec's (or init script's) captured stdout/stderr so
// one command cannot return an unbounded payload over the control socket.
const maxExecOutput = 64 << 10

// capOutput truncates b to maxExecOutput, appending a marker when it overflows,
// so a single exec cannot return an unbounded payload.
//
// @arg b The captured output bytes.
// @return string The output, truncated with a marker when it exceeds maxExecOutput.
//
// @testcase TestExecCapsOutput truncates output past the cap and marks it.
func capOutput(b []byte) string {
	if len(b) <= maxExecOutput {
		return string(b)
	}
	return string(b[:maxExecOutput]) + "\n... [output truncated]"
}
