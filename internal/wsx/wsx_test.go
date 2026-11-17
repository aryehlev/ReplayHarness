package wsx

import (
	"testing"

	"github.com/aryehlev/ReplayHarness/internal/model"
)

func TestEffective(t *testing.T) {
	for _, c := range []struct{ status, close, want int }{
		{101, 0, 200}, {101, 1000, 200}, {101, 1001, 200},
		{101, 1011, 500}, {101, 1008, 403}, {101, 1009, 400}, {101, 1003, 400},
		{101, 4404, 404}, {101, 4001, 400},
		{401, 0, 401}, {404, 0, 404}, {0, 0, 0},
	} {
		if got := Effective(model.Observed{Status: c.status, Close: c.close}); got != c.want {
			t.Errorf("status %d close %d: %d, want %d", c.status, c.close, got, c.want)
		}
	}
}
