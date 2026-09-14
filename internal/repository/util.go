package repository

import "strconv"

// itoa is a tiny shorthand for building numbered SQL placeholders.
func itoa(i int) string {
	return strconv.Itoa(i)
}

// joinComma joins SQL fragments with ", ".
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
