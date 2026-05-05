package replay

import "testing"

type fakeRand struct{ v float64 }

func (f fakeRand) Float64() float64 { return f.v }

func TestIsCapturable(t *testing.T) {
	tests := []struct {
		name      string
		shadow    bool
		method    string
		blacklist []string
		rate      float64
		rand      float64
		want      bool
	}{
		{"shadow always skipped", true, "/x.M", nil, 1.0, 0.0, false},
		{"blacklist hit", false, "/grpc.health.v1.Health/Check", []string{"/grpc.health."}, 1.0, 0.0, false},
		{"rate=0 never capture", false, "/x.M", nil, 0, 0.5, false},
		{"rate=1 always capture", false, "/x.M", nil, 1, 0.99, true},
		{"rate=0.1 rand=0.05 yes", false, "/x.M", nil, 0.1, 0.05, true},
		{"rate=0.1 rand=0.5 no", false, "/x.M", nil, 0.1, 0.5, false},
		{"prefix-only match", false, "/admin.foo.Bar/Baz", []string{"/admin."}, 1.0, 0.0, false},
		{"non-matching blacklist allows", false, "/api.v1.Foo/Bar", []string{"/grpc.health."}, 1.0, 0.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCapturable(tt.shadow, tt.method, tt.blacklist, tt.rate, fakeRand{tt.rand})
			if got != tt.want {
				t.Errorf("IsCapturable: got=%v want=%v", got, tt.want)
			}
		})
	}
}
