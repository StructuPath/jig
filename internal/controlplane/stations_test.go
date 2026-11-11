package controlplane

import (
	"os"
	"testing"
)

// The station list is the frozen chain in declared order, with the role's
// model on agent phases, the gate names, and the parallel group marked: the
// stock parallel factory exercises all of it.
func TestStationsFromSnapshotReadsTheStockFactory(t *testing.T) {
	source, err := os.ReadFile("../../examples/definitions/factory-parallel.yaml")
	if err != nil {
		t.Fatal(err)
	}
	stations, delivery := stationsFromSnapshot(string(source))
	if len(stations) != 11 {
		t.Fatalf("stations = %d, want the factory's 11 phases", len(stations))
	}
	byName := make(map[string]Station, len(stations))
	for _, station := range stations {
		byName[station.Name] = station
	}
	if stations[0].Name != "plan" || stations[1].Name != "build" {
		t.Fatalf("stations are not in declared order: %s, %s", stations[0].Name, stations[1].Name)
	}
	build := byName["build"]
	if build.Kind != "agent" || build.Owner != "builder" || build.Model != "opus" {
		t.Errorf("build = %+v, want an agent phase owned by builder on opus", build)
	}
	if !contains(build.Gates, "tests_intact") {
		t.Errorf("build gates = %v, want tests_intact among them", build.Gates)
	}
	test := byName["test"]
	if test.Kind != "code" || test.Owner != "" || test.Model != "" {
		t.Errorf("test = %+v, want a code phase with no role or model", test)
	}
	if !byName["review-security"].Parallel || byName["build"].Parallel {
		t.Errorf("parallel marks are wrong: review-security=%v build=%v",
			byName["review-security"].Parallel, byName["build"].Parallel)
	}
	if byName["review-risk"].If != "risk != low" {
		t.Errorf("review-risk guard = %q, want %q", byName["review-risk"].If, "risk != low")
	}
	if delivery == nil || delivery.HoldWhen != "risk != low" || !delivery.CIWait {
		t.Errorf("delivery = %+v, want the hold predicate and a CI wait", delivery)
	}
}

// A definition without a publish block still has stations; its delivery says
// nothing is held and nothing waits.
func TestStationsFromSnapshotWithoutPublish(t *testing.T) {
	source, err := os.ReadFile("../../examples/definitions/smoke.yaml")
	if err != nil {
		t.Fatal(err)
	}
	stations, delivery := stationsFromSnapshot(string(source))
	if len(stations) != 1 || stations[0].Name != "scout" {
		t.Fatalf("stations = %+v, want the one scout phase", stations)
	}
	if delivery == nil || delivery.HoldWhen != "" || delivery.CIWait {
		t.Errorf("delivery = %+v, want an empty policy", delivery)
	}
}

// A snapshot that does not parse costs the job detail its station list and
// nothing else.
func TestStationsFromSnapshotUnreadable(t *testing.T) {
	for _, snapshot := range []string{"", "not: [valid", "name: empty\n"} {
		stations, delivery := stationsFromSnapshot(snapshot)
		if stations != nil || delivery != nil {
			t.Errorf("snapshot %q: got %v, %v; want nothing", snapshot, stations, delivery)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
