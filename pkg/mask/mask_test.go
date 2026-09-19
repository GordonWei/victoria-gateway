package mask

import "testing"

func TestShape(t *testing.T) {
	got := Shape("Ab12-cd_34")
	want := "Aa00-aa_00"
	if got != want {
		t.Fatalf("Shape() = %q, want %q", got, want)
	}
}

func TestRedactLikelyCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "password assignment",
			in:   `connecting with password="hunter2pass"`,
			want: `connecting with password="aaaaaa0aaaa"`,
		},
		{
			name: "token colon",
			in:   `token: sk-abcDEF1234`,
			want: `token: aa-aaaAAA0000`,
		},
		{
			name: "bearer header",
			in:   `Authorization: Bearer abcXYZ.123456`,
			want: `Authorization: Bearer aaaAAA.000000`,
		},
		{
			name: "leaves unrelated content alone",
			in:   `connection refused to 10.0.0.5:5432`,
			want: `connection refused to 10.0.0.5:5432`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactLikelyCredentials(tc.in)
			if got != tc.want {
				t.Fatalf("RedactLikelyCredentials(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
