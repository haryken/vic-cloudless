package course

import "testing"

func TestPoolsMatchUbtPresets(t *testing.T) {
	cases := []struct {
		idx, voice  int
		first, last string
	}{
		{1, VoiceA, "ba:53:9e:c5:ba:10", "ba:53:9e:c5:ba:19"},
		{1, VoiceB, "ba:53:9e:c5:fe:10", "ba:53:9e:c5:fe:19"},
		{2, VoiceA, "ba:53:9e:c5:ef:10", "ba:53:9e:c5:ef:19"},
		{2, VoiceB, "ba:53:9e:c6:fe:10", "ba:53:9e:c6:fe:19"},
		{3, VoiceA, "ba:53:9e:c7:ef:10", "ba:53:9e:c7:ef:19"},
		{3, VoiceB, "ba:53:9e:c8:ef:10", "ba:53:9e:c8:ef:19"},
		{4, VoiceA, "ba:53:9e:c2:ef:10", "ba:53:9e:c2:ef:19"},
		{4, VoiceB, "ba:53:9e:c3:ef:10", "ba:53:9e:c3:ef:19"},
		{5, VoiceA, "ba:53:9e:c9:ef:10", "ba:53:9e:c9:ef:19"},
		{5, VoiceB, "ba:53:9e:c1:ef:10", "ba:53:9e:c1:ef:19"},
	}
	for _, tc := range cases {
		p := Pool(tc.idx, tc.voice)
		if len(p) != 10 || p[0] != tc.first || p[9] != tc.last {
			t.Errorf("Pool(%d,%d) = %v", tc.idx, tc.voice, p)
		}
	}
	if Pool(9, 0) != nil {
		t.Error("unknown course must have no pool")
	}
}

func TestPickMACStaysInPoolAndAvoidsCurrent(t *testing.T) {
	cur := "ba:53:9e:c3:ef:14"
	for i := 0; i < 50; i++ {
		m := PickMAC(4, VoiceB, cur)
		if !InPool(4, VoiceB, m) {
			t.Fatalf("picked %s outside pool", m)
		}
		if m == cur {
			t.Fatalf("picked the avoided MAC %s", m)
		}
	}
}

func TestShiftAcrossBooks(t *testing.T) {
	// Explorers book 0 has 8 units → next from unit 8 goes to book 1 unit 1.
	s, clamped := Shift(Selection{Idx: 1, Book: 0, Unit: 8}, 1)
	if clamped || s.Book != 1 || s.Unit != 1 {
		t.Fatalf("next from end of book: %+v clamped=%v", s, clamped)
	}
	s, clamped = Shift(s, -1)
	if clamped || s.Book != 0 || s.Unit != 8 {
		t.Fatalf("prev back into book 0: %+v clamped=%v", s, clamped)
	}
	s, clamped = Shift(Selection{Idx: 1, Book: 0, Unit: 1}, -1)
	if !clamped || s.Unit != 1 {
		t.Fatalf("prev at very start must clamp: %+v", s)
	}
	s, clamped = Shift(Selection{Idx: 5, Unit: 6}, 1)
	if !clamped || s.Unit != 6 {
		t.Fatalf("next at TOEIC end must clamp: %+v", s)
	}
}

func TestNormalizeClampsBadInput(t *testing.T) {
	s := Normalize(Selection{Idx: 42, Voice: 7, Book: 9, Unit: 99})
	if s.Idx != DefaultIdx || s.Voice != VoiceA || s.Book != 0 || s.Unit != 8 {
		t.Fatalf("Normalize = %+v", s)
	}
	if got := UnitName(Selection{Idx: 2, Book: 1, Unit: 2}); got != "Unit Eight My Town" {
		t.Fatalf("UnitName = %q", got)
	}
}

func TestShiftMessageAndDirection(t *testing.T) {
	before := Selection{Idx: 1, Book: 0, Unit: 8}
	after, clamped := Shift(before, 1)
	if got := ShiftMessage(before, after, 1, clamped); got != "Đã chuyển sách → Pre-Discovery B, unit tiếp theo: Unit Routines Review" {
		t.Fatalf("message = %q", got)
	}
	for in, want := range map[string]int{"next": 1, "Tiếp theo": 1, "prev": -1, "bài trước": -1, "x": 0} {
		if got := ParseDirection(in); got != want {
			t.Errorf("ParseDirection(%q) = %d, want %d", in, got, want)
		}
	}
}
