package config

import "testing"

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"lib/**/*.dart", "lib/a.dart", true},
		{"lib/**/*.dart", "lib/src/deep/a.dart", true},
		{"lib/**/*.dart", "lib/a.dart", true},          // ** matches zero segments
		{"lib/*.dart", "lib/a.dart", true},             // * does not cross /
		{"lib/*.dart", "lib/src/a.dart", false},        // * does not cross /
		{"**", "anything/at/all", true},                //
		{"**/*.g.dart", "lib/x.g.dart", true},          //
		{"**/*.g.dart", "lib/x.dart", false},           //
		{"lib/**", "lib/src/deep/x.dart", true},        //
		{"lib/**", "other/x.dart", false},              //
		{"lib/?.dart", "lib/a.dart", true},             //
		{"lib/?.dart", "lib/ab.dart", false},           //
		{"lib/[abc]*.dart", "lib/b_util.dart", true},   //
		{"lib/[!a]*.dart", "lib/b.dart", true},         //
		{"lib/[!a]*.dart", "lib/a.dart", false},        //
		{"exact/path.dart", "exact/path.dart", true},   //
		{"exact/path.dart", "exact/other.dart", false}, //
		{"lib/**/net*.dart", "lib/src/core/network.dart", true},
		// matchSegment corner cases via the public API.
		{"lib/**con**cat**.dart", "lib/concat.dart", true},  // ** glued to literals
		{"lib/a\\*b.dart", "lib/a*b.dart", true},            // escaped star is literal
		{"lib/a\\*b.dart", "lib/aXb.dart", false},           // escaped star matches no char
		{"lib/[a-c]x.dart", "lib/bx.dart", true},            // class range
		{"lib/[a-c]x.dart", "lib/dx.dart", false},           // outside range
		{"lib/[x", "lib/[x", false},                         // unterminated class
		{"lib/tra**iling.dart", "lib/traXiling.dart", true}, // ** mid-segment is two stars
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}
