package pressure

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestLevelFromPSI(t *testing.T) {
	cases := []struct {
		text string
		want Level
	}{
		{"some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", Normal},
		{"some avg10=12.50 avg60=3.00 avg300=1.00 total=5\nfull avg10=2.00 avg60=0.00 avg300=0.00 total=1\n", Warning},
		{"some avg10=40.00 avg60=3.00 avg300=1.00 total=5\nfull avg10=15.00 avg60=0.00 avg300=0.00 total=1\n", Critical},
	}
	for _, c := range cases {
		got, detail, ok := levelFromPSI(c.text)
		if !ok || got != c.want || detail == "" {
			t.Errorf("levelFromPSI(%q) = %v, %q, %v; want %v", c.text, got, detail, ok, c.want)
		}
	}
	if _, _, ok := levelFromPSI("nothing here"); ok {
		t.Error("judged text with no pressure lines")
	}
}

func TestLevelFromAvailableAndDarwin(t *testing.T) {
	if levelFromAvailable(50) != Normal || levelFromAvailable(8) != Warning || levelFromAvailable(3) != Critical {
		t.Error("available memory judged wrong")
	}
	if levelFromDarwin(1) != Normal || levelFromDarwin(2) != Warning || levelFromDarwin(4) != Critical {
		t.Error("macOS levels judged wrong")
	}
}

func TestALevelReadsAsItsName(t *testing.T) {
	data, err := json.Marshal(Reading{Level: Warning})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"level":"warning","source":"","detail":""}` {
		t.Fatalf("JSON = %s", data)
	}
}

func TestReadAndWatchOnThisMachine(t *testing.T) {
	r, err := Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Source == "" || r.Detail == "" {
		t.Fatalf("reading = %+v", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case first, ok := <-Watch(ctx, 50*time.Millisecond):
		if !ok || first.Source == "" {
			t.Fatalf("first watched reading = %+v, %v", first, ok)
		}
	case <-ctx.Done():
		t.Fatal("Watch sent no first reading")
	}
}
