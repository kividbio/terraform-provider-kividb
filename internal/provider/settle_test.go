package provider

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A resize that is accepted and then dropped on the way to the node leaves the
// database healthy at its old size. Terraform used to report that as
// "Provider produced inconsistent result after apply ... This is a bug in the
// provider", sending the practitioner to the wrong issue tracker for a
// control-plane problem. wantedBy is what turns it into a description of what
// happened.
func TestWantedByCatchesAChangeThatNeverLanded(t *testing.T) {
	state := instanceModel{DataSizeGB: types.Int64Value(1)}
	plan := instanceModel{DataSizeGB: types.Int64Value(2)}

	want := wantedBy(&plan, &state)
	if want == nil {
		t.Fatal("a changed data_size_gb must produce a condition to wait for")
	}

	// What actually came back: healthy, and still 1 GB.
	err := want(&Instance{DataSizeGB: 1, Status: "running"})
	if err == nil {
		t.Fatal("a database still at the old size must not count as settled")
	}
	for _, needle := range []string{"data_size_gb", "1", "2"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("the error must name %q so the reader knows what did not happen: %v", needle, err)
		}
	}

	if err := want(&Instance{DataSizeGB: 2, Status: "running"}); err != nil {
		t.Errorf("a database that reached the requested size is settled: %v", err)
	}
}

// Only what the plan changed is checked. Anything else would turn server-side
// normalisation the practitioner never asked about into a failed apply.
func TestWantedByIgnoresWhatThePlanDidNotChange(t *testing.T) {
	state := instanceModel{
		DataSizeGB:     types.Int64Value(4),
		ThroughputMode: types.StringValue("standard"),
		Tier:           types.StringValue("essentials"),
	}
	plan := state

	if want := wantedBy(&plan, &state); want != nil {
		t.Fatal("an update that changes none of the checked attributes needs no condition")
	}

	plan.ThroughputMode = types.StringValue("high")
	want := wantedBy(&plan, &state)
	if want == nil {
		t.Fatal("a changed throughput_mode must be waited for")
	}
	// Tier differs from the plan's value here, but the plan did not change it,
	// so it must not be part of the condition.
	err := want(&Instance{ThroughputMode: "high", Tier: "pro", DataSizeGB: 999})
	if err != nil {
		t.Errorf("only the changed attribute should be checked: %v", err)
	}
}

func TestWantedByCoversEveryInPlaceAttribute(t *testing.T) {
	name := "after"
	cases := []struct {
		what  string
		state instanceModel
		plan  instanceModel
		got   Instance
	}{
		{
			"tier",
			instanceModel{Tier: types.StringValue("essentials")},
			instanceModel{Tier: types.StringValue("pro")},
			Instance{Tier: "essentials"},
		},
		{
			"replica_count",
			instanceModel{ReplicaCount: types.Int64Value(1)},
			instanceModel{ReplicaCount: types.Int64Value(3)},
			Instance{ReplicaCount: 1},
		},
		{
			"name",
			instanceModel{Name: types.StringValue("before")},
			instanceModel{Name: types.StringValue("after")},
			Instance{Name: nil},
		},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			want := wantedBy(&c.plan, &c.state)
			if want == nil {
				t.Fatalf("a changed %s must be waited for", c.what)
			}
			if err := want(&c.got); err == nil {
				t.Fatalf("%s did not change on the server; that must not read as settled", c.what)
			} else if !strings.Contains(err.Error(), c.what) {
				t.Errorf("the error must name %s: %v", c.what, err)
			}
		})
	}
	// And the name case settles once the server reports it.
	want := wantedBy(
		&instanceModel{Name: types.StringValue("after")},
		&instanceModel{Name: types.StringValue("before")},
	)
	if err := want(&Instance{Name: &name}); err != nil {
		t.Errorf("a completed rename is settled: %v", err)
	}
}

// An unknown planned value cannot be compared against anything, and treating it
// as a requirement would fail every apply that left an attribute to the server.
func TestWantedBySkipsUnknownPlanValues(t *testing.T) {
	state := instanceModel{DataSizeGB: types.Int64Value(1)}
	plan := instanceModel{DataSizeGB: types.Int64Unknown()}
	if want := wantedBy(&plan, &state); want != nil {
		if err := want(&Instance{DataSizeGB: 1}); err != nil {
			t.Errorf("an unknown planned value must not be asserted: %v", err)
		}
	}
}
