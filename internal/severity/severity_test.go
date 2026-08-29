package severity

import "testing"

func TestNumber(t *testing.T) {
	cases := []struct {
		text string
		want int64
	}{
		{"DEBUG", 5},
		{"debug", 5},
		{"Debug", 5},
		{"INFO", 9},
		{"WARN", 13},
		{"ERROR", 17},
		{"FATAL", 21},
		{"", 0},
		{"bogus", 0},
	}
	for _, c := range cases {
		if got := Number(c.text); got != c.want {
			t.Errorf("Number(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestAllAscending(t *testing.T) {
	for i := 1; i < len(All); i++ {
		if All[i-1].Number >= All[i].Number {
			t.Errorf("All[%d].Number=%d not strictly ascending before All[%d].Number=%d",
				i-1, All[i-1].Number, i, All[i].Number)
		}
	}
}
