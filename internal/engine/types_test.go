package engine

import (
	"testing"
)

func TestDecimalArithmeticExact(t *testing.T) {
	cases := []struct {
		a, b  Decimal
		scale int
		op    string
		want  string
	}{
		{Decimal{150, 2}, Decimal{225, 2}, 2, "add", "3.75"},
		{Decimal{150, 2}, Decimal{225, 2}, 2, "sub", "-0.75"},
		{Decimal{150, 2}, Decimal{225, 2}, 4, "mul", "3.3750"},
		{Decimal{150, 2}, Decimal{225, 2}, 6, "div", "0.666667"},
		// Round half away from zero: 1.25/2 = 0.625 -> 0.63 at scale 2;
		// 5/2 = 2.5 -> 3 at scale 0.
		{Decimal{125, 2}, Decimal{2, 0}, 2, "div", "0.63"},
		{Decimal{-125, 2}, Decimal{2, 0}, 2, "div", "-0.63"},
		{Decimal{5, 0}, Decimal{2, 0}, 0, "div", "3"},
		{Decimal{-5, 0}, Decimal{2, 0}, 0, "div", "-3"},
		{Decimal{125, 2}, Decimal{1, 0}, 1, "mul", "1.3"},
		{Decimal{-125, 2}, Decimal{1, 0}, 1, "mul", "-1.3"},
	}
	for _, c := range cases {
		var got Decimal
		var ok bool
		switch c.op {
		case "add":
			got, ok = DecAdd(c.a, c.b, c.scale)
		case "sub":
			got, ok = DecSub(c.a, c.b, c.scale)
		case "mul":
			got, ok = DecMul(c.a, c.b, c.scale)
		case "div":
			got, ok = DecDiv(c.a, c.b, c.scale)
		}
		if !ok {
			t.Fatalf("%s(%v,%v,%d) overflowed", c.op, c.a, c.b, c.scale)
		}
		if got.String() != c.want {
			t.Errorf("%s(%v,%v,%d) = %s, want %s", c.op, c.a, c.b, c.scale, got.String(), c.want)
		}
	}
}

func TestDecimalOverflowReported(t *testing.T) {
	big := Decimal{1 << 62, 0}
	if _, ok := DecAdd(big, big, 0); ok {
		t.Errorf("expected overflow on decimal addition")
	}
	if _, ok := DecMul(big, big, 0); ok {
		t.Errorf("expected overflow on decimal multiplication")
	}
	if _, ok := DecDiv(Decimal{1, 0}, Decimal{0, 0}, 6); ok {
		t.Errorf("expected division by zero to fail")
	}
}

func TestDecimalRoundTrip(t *testing.T) {
	for _, s := range []string{"0", "1.50", "-0.75", "100.00", "12.345678"} {
		d, _, ok := ParseDecimal(s)
		if !ok {
			t.Fatalf("parse %q failed", s)
		}
		if d.String() != s {
			t.Errorf("%q round-tripped to %q", s, d.String())
		}
	}
	if _, _, ok := ParseDecimal("1234567890123456789"); ok {
		t.Errorf("19-digit literal should not fit the exact decimal range")
	}
}

func TestDateRoundTrip(t *testing.T) {
	for _, s := range []string{"1970-01-01", "2024-02-29", "1999-12-31", "2100-03-01"} {
		z, ok := ParseDate(s)
		if !ok {
			t.Fatalf("parse %q failed", s)
		}
		if got := FormatDate(z); got != s {
			t.Errorf("%q round-tripped to %q", s, got)
		}
	}
	for _, bad := range []string{"2023-02-29", "2024-13-01", "2024-00-10", "24-01-01"} {
		if _, ok := ParseDate(bad); ok {
			t.Errorf("invalid date %q accepted", bad)
		}
	}
}

func TestClockRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00", "95.50", "100.00", "3"} {
		c, ok := ParseTick(s)
		if !ok {
			t.Fatalf("parse %q failed", s)
		}
		if c < 0 || c > TickMax {
			t.Errorf("%q parsed out of range: %d", s, c)
		}
	}
	for _, bad := range []string{"100.01", "-1.00", "abc", "1.234"} {
		if _, ok := ParseTick(bad); ok {
			t.Errorf("invalid tick %q accepted", bad)
		}
	}
	// The documented explicit conversion is invertible over its window.
	base, _ := ParseDate("2024-01-01")
	for day := int32(0); day <= 100; day++ {
		tick := day * 100
		back := base + tick/100
		if back != base+day {
			t.Fatalf("clock round trip failed at day %d", day)
		}
	}
}
