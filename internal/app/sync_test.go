package app

import (
	"slices"
	"testing"

	"github.com/greeddj/imapsync-go/internal/config"
)

func TestComputeEffectiveWorkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		workers int
		maxConn int
		planCnt int
		want    int
	}{
		{name: "underBudget", workers: 4, maxConn: 15, planCnt: 20, want: 4},
		{name: "atBudgetReservesOneForPlanner", workers: 15, maxConn: 15, planCnt: 20, want: 14},
		{name: "overBudgetClampedToMaxConnMinusOne", workers: 20, maxConn: 15, planCnt: 20, want: 14},
		{name: "maxConnOneFloorsToOne", workers: 1, maxConn: 1, planCnt: 20, want: 1},
		{name: "unlimitedMaxConn", workers: 4, maxConn: 0, planCnt: 20, want: 4},
		{name: "fewerPlansThanWorkers", workers: 4, maxConn: 15, planCnt: 2, want: 2},
		{name: "zeroPlansFloorsToOne", workers: 4, maxConn: 15, planCnt: 0, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := computeEffectiveWorkers(tt.workers, tt.maxConn, tt.planCnt); got != tt.want {
				t.Errorf("computeEffectiveWorkers(%d, %d, %d) = %d, want %d",
					tt.workers, tt.maxConn, tt.planCnt, got, tt.want)
			}
		})
	}
}

func TestDedupeMappings_keepsFirstOccurrence(t *testing.T) {
	t.Parallel()

	in := []config.DirectoryMapping{
		{Source: "A", Destination: "X"},
		{Source: "A", Destination: "Y"},
		{Source: "B", Destination: "Z"},
	}
	out, dropped := dedupeMappings(in)

	wantOut := []config.DirectoryMapping{
		{Source: "A", Destination: "X"},
		{Source: "B", Destination: "Z"},
	}
	if !slices.Equal(out, wantOut) {
		t.Errorf("out=%+v, want %+v", out, wantOut)
	}
	wantDropped := []config.DirectoryMapping{{Source: "A", Destination: "Y"}}
	if !slices.Equal(dropped, wantDropped) {
		t.Errorf("dropped=%+v, want %+v", dropped, wantDropped)
	}
}

func TestDedupeMappings_emptyInput(t *testing.T) {
	t.Parallel()

	out, dropped := dedupeMappings(nil)
	if len(out) != 0 {
		t.Errorf("out=%+v, want empty", out)
	}
	if dropped != nil {
		t.Errorf("dropped=%+v, want nil", dropped)
	}
}

func TestDedupeMappings_noDuplicates_passthrough(t *testing.T) {
	t.Parallel()

	in := []config.DirectoryMapping{
		{Source: "A", Destination: "X"},
		{Source: "B", Destination: "Y"},
		{Source: "C", Destination: "Z"},
	}
	out, dropped := dedupeMappings(in)

	if !slices.Equal(out, in) {
		t.Errorf("out=%+v, want %+v", out, in)
	}
	if dropped != nil {
		t.Errorf("dropped=%+v, want nil", dropped)
	}
}

// TestDedupeMappings_parentExpandsBeforeExplicitChild mirrors the real bug:
// a parent mapping's subfolder expansion produces an entry that an explicit
// later mapping would re-add. The first occurrence (the expansion) wins.
func TestDedupeMappings_parentExpandsBeforeExplicitChild(t *testing.T) {
	t.Parallel()

	in := []config.DirectoryMapping{
		{Source: "jira", Destination: "jira"},
		{Source: "jira/DEVOPS", Destination: "jira/DEVOPS"},
		{Source: "jira/INTERNAL", Destination: "jira/INTERNAL"},
		{Source: "jira/DEVOPS", Destination: "DEVOPS"},
		{Source: "jira/INTERNAL", Destination: "INTERNAL"},
	}
	out, dropped := dedupeMappings(in)
	if len(out) != 3 {
		t.Errorf("len(out)=%d, want 3, got %+v", len(out), out)
	}
	if len(dropped) != 2 {
		t.Errorf("len(dropped)=%d, want 2, got %+v", len(dropped), dropped)
	}
}
