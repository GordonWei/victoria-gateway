package glob

import "testing"

func TestCompileMatches(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"web-*", "web-01", true},
		{"*prod*", "/env/prod/svc", true},
		{"web-[0-9]", "web-7", true},
		{"web-[0-9]", "web-x", false},
		{"a.b", "axb", false},
		{"h?st", "host", true},
	}
	for _, tc := range cases {
		re, err := Compile(tc.pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", tc.pattern, err)
		}
		if got := re.MatchString(tc.value); got != tc.want {
			t.Errorf("%q vs %q = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	for _, p := range []string{"web-[0-9", "[", "x[z-a]"} {
		if err := Validate(p); err == nil {
			t.Errorf("Validate(%q) = nil, want error", p)
		}
	}
	for _, p := range []string{"", "plain", "*", "a[bc]d"} {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", p, err)
		}
	}
}

func TestValidateMatchersNamesLabel(t *testing.T) {
	err := ValidateMatchers(map[string]string{"host": "web-[0-9"})
	if err == nil || !contains(err.Error(), `"host"`) {
		t.Fatalf("err = %v, want mention of host", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
