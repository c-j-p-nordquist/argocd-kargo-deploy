// Package untrusted handles text that came from a cluster.
//
// Names, annotations, image references and messages read from a cluster are
// written by whoever can create objects there. They must not be able to pose
// as instructions to an agent reading this tool's output, or break its format.
package untrusted

import "strings"

// maxLength is the longest string Clean returns, before its ellipsis.
const maxLength = 120

// Clean returns s reduced to printable ASCII and a bounded length.
func Clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxLength {
			b.WriteString("...")
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
