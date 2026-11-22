package core

import "fmt"

// RefContextBlock renders a @file reference as an API context block
// (snapshotted content, optionally line-ranged).
func RefContextBlock(r FileRef) string {
	label := r.Path
	if r.Start > 0 {
		label = fmt.Sprintf("%s#L%d-%d", r.Path, r.Start, r.End)
	}
	return fmt.Sprintf("[file: %s]\n%s", label, r.Content)
}
