// internal/mx/mx_test.go
package mx

import "testing"

func TestStripBOM(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"with BOM", append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"a":1}`)...), []byte(`{"a":1}`)},
		{"without BOM", []byte(`{"a":1}`), []byte(`{"a":1}`)},
		{"empty", []byte{}, []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripBOM(tc.in)
			if string(got) != string(tc.want) {
				t.Errorf("stripBOM(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}