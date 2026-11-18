package mic

import "testing"

func TestTheCaptureStatusIsReadAsTheSpeakersIs(t *testing.T) {
	for _, c := range []struct {
		status string
		free   bool
		owner  int
	}{
		{"closed\n", true, 0},
		{"state: RUNNING\nowner_pid   : 2195\ntrigger_time: 1.0\n", false, 2195},
		{"state: PREPARED\nowner_pid: 7\n", false, 7},
		{"something unfamiliar\n", true, 0},
	} {
		if captureFree(c.status) != c.free || captureOwner(c.status) != c.owner {
			t.Fatalf("%q: free %v owner %d", c.status, captureFree(c.status), captureOwner(c.status))
		}
	}
}
