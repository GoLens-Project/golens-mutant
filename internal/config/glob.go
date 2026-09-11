package config

import "strings"

// MatchGlob reports whether name matches pattern. Both use '/' as the
// path separator (package-relative, already-cleaned paths). Supported
// syntax per pattern segment: '*' (no separator crossing), '?' (one
// character), '[...]' (character class), and the special segment '**'
// matching zero or more whole segments.
func MatchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case "**":
			// '**' consumes zero or more segments: try every suffix.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		default:
			if len(segs) == 0 || !matchSegment(pat[0], segs[0]) {
				return false
			}
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// matchSegment matches one path segment against one pattern segment,
// equivalent to path.Match but guaranteed not to treat '/' specially
// (segments never contain separators).
func matchSegment(pattern, seg string) bool {
	// Fast path: no wildcards.
	if !strings.ContainsAny(pattern, "*?[\\") {
		return pattern == seg
	}
	p, s := pattern, seg
	for len(p) > 0 {
		switch p[0] {
		case '*':
			// Collapse consecutive stars; try suffixes.
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if matchSegment(p, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
		case '[':
			rest, ok := matchClass(p, s)
			if !ok {
				return false
			}
			p, s = rest, s[1:]
			continue
		case '\\':
			if len(p) < 2 {
				return false
			}
			if len(s) == 0 || s[0] != p[1] {
				return false
			}
			p = p[2:]
			if len(s) > 0 {
				s = s[1:]
			}
			continue
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
			p, s = p[1:], s[1:]
			continue
		}
		p, s = p[1:], s[1:]
	}
	return len(s) == 0
}

// matchClass matches a leading '[...]' class in p against s[0]. It
// returns the pattern remainder and whether the class matched.
func matchClass(p, s string) (string, bool) {
	// p starts with '['. Find the closing ']'.
	i := 1
	if i < len(p) && (p[i] == '^' || p[i] == '!') {
		i++
	}
	if i < len(p) && p[i] == ']' {
		i++
	}
	for i < len(p) && p[i] != ']' {
		i++
	}
	if i >= len(p) || len(s) == 0 {
		return p, false
	}
	class := p[1:i]
	negate := false
	if class[0] == '^' || class[0] == '!' {
		negate = true
		class = class[1:]
	}
	matched := false
	for j := 0; j < len(class); j++ {
		if j+2 < len(class) && class[j+1] == '-' {
			if class[j] <= s[0] && s[0] <= class[j+2] {
				matched = true
			}
			j += 2
		} else if class[j] == s[0] {
			matched = true
		}
	}
	if negate {
		matched = !matched
	}
	return p[i+1:], matched
}
