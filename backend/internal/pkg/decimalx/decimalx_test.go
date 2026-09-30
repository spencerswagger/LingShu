package decimalx

import "testing"

func TestRound5(t *testing.T) {
	if got := Round5(0.094723); got != 0.09472 {
		t.Fatalf("Round5(0.094723)=%v want 0.09472", got)
	}
}

func TestRound2(t *testing.T) {
	if got := Round2(499.905283); got != 499.91 {
		t.Fatalf("Round2(499.905283)=%v want 499.91", got)
	}
}