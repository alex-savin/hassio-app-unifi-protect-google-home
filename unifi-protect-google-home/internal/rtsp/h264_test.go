package rtsp

import "testing"

func TestProfileLevelIDFromSPS(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{
			name: "baseline 3.1 with NAL header",
			in:   []byte{0x67, 0x42, 0xe0, 0x1f},
			want: "42e01f",
		},
		{
			name: "high 4.0 with NAL header",
			in:   []byte{0x67, 0x64, 0x00, 0x28},
			want: "640028",
		},
		{
			name: "main 4.1 stripped header",
			in:   []byte{0x4d, 0x40, 0x29},
			want: "4d4029",
		},
		{
			name: "empty falls back",
			in:   nil,
			want: defaultProfileLevelID,
		},
		{
			name: "too short falls back",
			in:   []byte{0x67, 0x42},
			want: defaultProfileLevelID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := profileLevelIDFromSPS(tc.in)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
