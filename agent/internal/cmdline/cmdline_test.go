package cmdline

import "testing"

func TestParseString(t *testing.T) {
	tests := []struct {
		line string
		want Params
	}{
		{
			line: "BOOT_IMAGE=/vmlinuz fog_server=http://10.0.0.1 fog_action=deploy fog_host=aa:bb:cc:dd:ee:ff",
			want: Params{FogServer: "http://10.0.0.1", FogAction: "deploy", FogHost: "aa:bb:cc:dd:ee:ff"},
		},
		{
			line: "fog_server=http://fog fog_debug=1",
			want: Params{FogServer: "http://fog", FogDebug: true},
		},
		{
			line: "",
			want: Params{},
		},
	}
	for _, tc := range tests {
		got := ParseString(tc.line)
		if *got != tc.want {
			t.Errorf("ParseString(%q) = %+v, want %+v", tc.line, *got, tc.want)
		}
	}
}
