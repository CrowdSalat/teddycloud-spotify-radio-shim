package process

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil): got %d, want 0", got)
	}

	for _, code := range []int{1, 10} {
		err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
		if err == nil {
			t.Fatalf("expected /bin/sh -c exit %d to fail", code)
		}

		if got := ExitCode(err); got != code {
			t.Errorf("ExitCode(err): got %d, want %d", got, code)
		}
	}

	if got := ExitCode(errors.New("not an exit error")); got != -1 {
		t.Errorf("ExitCode(non-exit error): got %d, want -1", got)
	}
}
