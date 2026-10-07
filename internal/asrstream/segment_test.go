package asrstream

import "testing"

func TestPlanSegments(t *testing.T) {
	cases := []struct {
		name                             string
		durationMS, segmentMS, overlapMS int64
		want                             []Segment
	}{
		{"shorter than one window", 90000, 300000, 2000,
			[]Segment{{1, 0, 90000}}},
		{"exactly one window", 300000, 300000, 2000,
			[]Segment{{1, 0, 300000}}},
		{"two full windows overlap", 600000, 300000, 2000,
			[]Segment{{1, 0, 300000}, {2, 298000, 598000}, {3, 596000, 600000}}},
		{"empty recording", 0, 300000, 2000, nil},
		{"zero overlap", 400000, 300000, 0,
			[]Segment{{1, 0, 300000}, {2, 300000, 400000}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanSegments(tc.durationMS, tc.segmentMS, tc.overlapMS)
			if err != nil {
				t.Fatalf("PlanSegments: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("segments = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("segment %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestPlanSegmentsRejectsInvalidWindows(t *testing.T) {
	for name, args := range map[string][3]int64{
		"zero segment length": {60000, 0, 0},
		"negative segment":    {60000, -1, 0},
		"negative overlap":    {60000, 30000, -1},
		"overlap equals":      {60000, 30000, 30000},
		"overlap larger":      {60000, 30000, 40000},
	} {
		if _, err := PlanSegments(args[0], args[1], args[2]); err == nil {
			t.Errorf("%s: PlanSegments accepted an invalid window", name)
		}
	}
}
