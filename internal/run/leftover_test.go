package run

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestProcargsEnvironmentReturnsOnlyEnvironment(t *testing.T) {
	t.Run("arguments and environment", func(t *testing.T) {
		var buf []byte
		buf = append(buf, 2, 0, 0, 0) // argc 2, little-endian
		buf = append(buf, "/bin/x"...)
		buf = append(buf, 0, 0, 0, 0) // exec path terminator plus padding
		buf = append(buf, "x"...)
		buf = append(buf, 0)
		buf = append(buf, "PI_WORKER_RUN=abc"...)
		buf = append(buf, 0)
		buf = append(buf, "A=1"...)
		buf = append(buf, 0)
		buf = append(buf, "B=2"...)
		buf = append(buf, 0)
		buf = append(buf, 0, 0) // trailing NULs

		got := procargsEnvironment(buf)
		want := []string{"A=1", "B=2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("procargsEnvironment = %v, want %v", got, want)
		}
	})

	t.Run("empty argument does not shift environment", func(t *testing.T) {
		var buf []byte
		buf = append(buf, 3, 0, 0, 0) // argc 3, little-endian
		buf = append(buf, "/bin/x"...)
		buf = append(buf, 0, 0, 0) // exec path terminator plus padding
		buf = append(buf, "x"...)
		buf = append(buf, 0)
		buf = append(buf, 0) // empty argument, still counted by argc
		buf = append(buf, "PI_WORKER_RUN=abc"...)
		buf = append(buf, 0)
		buf = append(buf, "A=1"...)
		buf = append(buf, 0)
		buf = append(buf, 0) // trailing NUL

		got := procargsEnvironment(buf)
		want := []string{"A=1"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("procargsEnvironment = %v, want %v", got, want)
		}
	})
}

func TestProcargsEnvironmentRejectsShortBuffers(t *testing.T) {
	var tooFewArgs []byte
	tooFewArgs = append(tooFewArgs, 5, 0, 0, 0) // argc 5, little-endian
	tooFewArgs = append(tooFewArgs, "/bin/x"...)
	tooFewArgs = append(tooFewArgs, 0, 0)
	tooFewArgs = append(tooFewArgs, "x"...)
	tooFewArgs = append(tooFewArgs, 0)
	tooFewArgs = append(tooFewArgs, "y"...)
	tooFewArgs = append(tooFewArgs, 0)

	cases := map[string][]byte{
		"nil":                  nil,
		"two bytes":            {1, 0},
		"fewer args than argc": tooFewArgs,
	}
	for name, buf := range cases {
		t.Run(name, func(t *testing.T) {
			if got := procargsEnvironment(buf); got != nil {
				t.Fatalf("procargsEnvironment = %v, want nil", got)
			}
		})
	}
}

func TestLeftoverNamePrefersProgramFile(t *testing.T) {
	cases := []struct {
		name    string
		exe     string
		exeErr  error
		proc    string
		nameErr error
		want    string
	}{
		{"program file wins over thread name", "/usr/bin/node", nil, "MainThread", nil, "node"},
		{"empty program file falls back to name", "", nil, "node", nil, "node"},
		{"program file error falls back to name", "/usr/bin/node", errors.New("x"), "node", nil, "node"},
		{"both errors yield empty", "", errors.New("x"), "node", errors.New("y"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leftoverName(tc.exe, tc.exeErr, tc.proc, tc.nameErr); got != tc.want {
				t.Fatalf("leftoverName(%q, %v, %q, %v) = %q, want %q", tc.exe, tc.exeErr, tc.proc, tc.nameErr, got, tc.want)
			}
		})
	}
}

func TestCreatedFloorAllowsCoarseCreationTimes(t *testing.T) {
	since := time.UnixMilli(10_300)
	want := int64(8_000)
	if got := createdFloor(since); got != want {
		t.Fatalf("createdFloor(%v) = %d, want %d", since, got, want)
	}
}

func TestHasRunMarkerExactMatch(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		id   string
		want bool
	}{
		{"exact", []string{"PI_WORKER_RUN=abc"}, "abc", true},
		{"superset value", []string{"PI_WORKER_RUN=abcd"}, "abc", false},
		{"nested in another value", []string{"X=PI_WORKER_RUN=abc"}, "abc", false},
		{"empty value", []string{"PI_WORKER_RUN="}, "abc", false},
		{"nil env", nil, "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasRunMarker(tc.env, tc.id); got != tc.want {
				t.Fatalf("hasRunMarker(%v, %q) = %v, want %v", tc.env, tc.id, got, tc.want)
			}
		})
	}
}
