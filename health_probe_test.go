package runkube

import "testing"

// TestHealthProcessesHaveProbes guards the wiring: every control-plane
// component that can be probed must carry a Process.Status callback, and the
// node processes must come along too.
func TestHealthProcessesHaveProbes(t *testing.T) {
	c := testControlPlane(t)
	ps := c.healthProcesses()
	if len(ps) == 0 {
		t.Fatal("no processes")
	}
	for _, p := range ps {
		if p.Status == nil {
			t.Errorf("%s has no probe", p.Tag)
		}
	}
	if c.FormationStatus() == nil {
		t.Error("no formation status policy")
	}
}
