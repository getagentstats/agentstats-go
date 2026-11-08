package cli

import (
	"errors"
	"io/fs"
	"testing"
)

func TestErrorTextAndFlagNames(t *testing.T) {
	tr := &Tracker{opts: Options{
		ErrorText: func(e ErrorInfo) string {
			var pe *fs.PathError
			if errors.As(e.Err, &pe) {
				return e.Command + ": " + pe.Op + " failed"
			}
			return ""
		},
		FlagNames: func(_ string, flags []string) []string { return flags[1:] },
	}, flags: []string{"--token", "--env"}}
	tr.Error(&fs.PathError{Op: "open", Path: "/home/jane/secrets.env", Err: fs.ErrNotExist})
	if got := tr.errorText("db seed", 1); got != "db seed: open failed" {
		t.Fatalf("errorText = %q", got)
	}
	if got := tr.flagNames("db seed"); len(got) != 1 || got[0] != "--env" {
		t.Fatalf("flagNames = %v", got)
	}

	off := &Tracker{opts: Options{NoErrors: true, NoFlags: true}, flags: []string{"--env"}}
	off.Error(errors.New("secret"))
	if off.errorText("x", 1) != "" || len(off.flagNames("x")) != 0 {
		t.Fatal("NoErrors/NoFlags still sent data")
	}
	panics := &Tracker{opts: Options{ErrorText: func(ErrorInfo) string { panic("bug") }}}
	panics.Error(errors.New("secret"))
	if panics.errorText("x", 1) != "" {
		t.Fatal("a panicking ErrorText kept text")
	}
}
