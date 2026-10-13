// Package course is the English-teaching preset ported from ubt_speech_demo
// Self-Control (Otto device_identity_presets.h / otto_course_units.h): each
// course has two voices, each voice a pool of tenclass MACs bound to a
// teaching agent, plus the book/unit list the agent asks about via MCP.
package course

import (
	"crypto/rand"
	"strings"
)

// Voice indices (ubt VOICE_A / VOICE_B).
const (
	VoiceA = 0 // Vietnamese / bilingual
	VoiceB = 1 // English
)

type Voice struct {
	Label string
	Pool  []string
}

type Book struct {
	Name  string
	Units []string
}

type Course struct {
	Idx    int
	ID     string
	Name   string
	Voices [2]Voice
	Books  []Book
}

func pool(prefix string) []string {
	out := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		out = append(out, prefix+":1"+string(rune('0'+i)))
	}
	return out
}

func numberedUnits(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "Unit " + string(rune('1'+i))
	}
	return out
}

// Catalog indices match ubt preset_mac_idx 1..5 so agent prompts stay valid.
var catalog = []Course{
	{
		Idx: 1, ID: "explorers", Name: "Explorers",
		Voices: [2]Voice{
			{"Song ngữ Anh – Việt", pool("ba:53:9e:c5:ba")},
			{"Tiếng Anh", pool("ba:53:9e:c5:fe")},
		},
		Books: []Book{
			{"Pre-Discovery A", []string{
				"Unit Routines", "Unit Welcome The Magic Forest", "Unit One Let's Draw",
				"Unit Two Let's Play", "Unit Review One", "Unit Three The Big Monster",
				"Unit Special Day One", "Unit Special Day Two",
			}},
			{"Pre-Discovery B", []string{
				"Unit Routines Review", "Unit Four My Family", "Unit Review Two",
				"Unit Five Where's My Bird?", "Unit Midterm Test", "Unit Six Let's Tidy Up!",
				"Unit Review Three", "Unit Special Day Three", "Unit Special Day Four",
				"Unit Review Four", "Unit Final Test",
			}},
			{"PRE-SPARK A", []string{
				"Unit Routines", "Unit Welcome Let's Remember", "Unit One The Surprise",
				"Unit Two The Brown Mouse", "Unit Review One", "Unit Three Where Is Greenman?",
				"Unit Special Day One", "Unit Special Day Two",
			}},
			{"SPARK Two A", []string{
				"Unit Our Friends", "Unit One Weather", "Unit Two Our Families",
				"Unit Three Our Places", "Unit Four Our Colors and Shapes",
			}},
			{"SPARK Three A", []string{
				"Unit Hello Again", "Unit One Our Day", "Unit Two Dinner Time",
				"Unit Three With My Friends", "Unit Four Our Colors and Shapes",
				"Unit Four Our Faces",
			}},
		},
	},
	{
		Idx: 2, ID: "younginnovators", Name: "Young Innovators",
		Voices: [2]Voice{
			{"Tiếng Việt", pool("ba:53:9e:c5:ef")},
			{"Tiếng Anh", pool("ba:53:9e:c6:fe")},
		},
		Books: []Book{
			{"QUEST One A", []string{
				"Unit One Hello", "Unit Two My School", "Unit Three Favourite Toys",
				"Unit Four My Family", "Unit Five Our Pets", "Unit Six My Face",
			}},
			{"QUEST Two B", []string{
				"Unit Seven At the Farm", "Unit Eight My Town", "Unit Nine Our Clothes",
				"Unit Ten Our Hobbies", "Unit Eleven My Birthday", "Unit Twelve Our Holiday",
			}},
			{"QUEST Three A", []string{
				"Unit Hello", "Unit One Family Matters", "Unit Two Home Sweet Home",
				"Unit Values One and Two", "Unit Three A Day in the Life", "Unit Four In the City",
				"Unit Values Three and Four", "Unit YLE Movers Listening Skills Practice",
			}},
		},
	},
	{
		Idx: 3, ID: "futureleaders", Name: "Future Leaders",
		Voices: [2]Voice{
			{"Tiếng Việt", pool("ba:53:9e:c7:ef")},
			{"Tiếng Anh", pool("ba:53:9e:c8:ef")},
		},
		Books: []Book{
			{"Summit One A", []string{
				"Unit Welcome", "Unit One Having a Good Time", "Unit Two Spending Money",
				"Unit Three We Are What We Eat", "Unit Four All in the Family",
				"Unit Five No Place Like Home", "Unit Six Friends Forever",
			}},
		},
	},
	{
		Idx: 4, ID: "ielts", Name: "IELTS",
		Voices: [2]Voice{
			{"Tiếng Việt", pool("ba:53:9e:c2:ef")},
			{"Tiếng Anh", pool("ba:53:9e:c3:ef")},
		},
		Books: []Book{{"IELTS", numberedUnits(6)}},
	},
	{
		Idx: 5, ID: "toeic", Name: "TOEIC",
		Voices: [2]Voice{
			{"Tiếng Việt", pool("ba:53:9e:c9:ef")},
			{"Tiếng Anh", pool("ba:53:9e:c1:ef")},
		},
		Books: []Book{{"TOEIC", numberedUnits(6)}},
	},
}

const DefaultIdx = 1

// All returns the catalog in display order.
func All() []Course { return catalog }

// Get returns the course with preset index idx (1..5).
func Get(idx int) (Course, bool) {
	for _, c := range catalog {
		if c.Idx == idx {
			return c, true
		}
	}
	return Course{}, false
}

func normVoice(v int) int {
	if v == VoiceB {
		return VoiceB
	}
	return VoiceA
}

// Pool returns the MAC pool for a course + voice (nil for unknown course).
func Pool(idx, voice int) []string {
	c, ok := Get(idx)
	if !ok {
		return nil
	}
	return c.Voices[normVoice(voice)].Pool
}

// PickMAC returns a random pool MAC, avoiding the current one when possible.
func PickMAC(idx, voice int, avoid string) string {
	p := Pool(idx, voice)
	if len(p) == 0 {
		return ""
	}
	avoid = strings.ToLower(strings.TrimSpace(avoid))
	var b [1]byte
	_, _ = rand.Read(b[:])
	start := int(b[0]) % len(p)
	for i := 0; i < len(p); i++ {
		if m := p[(start+i)%len(p)]; m != avoid {
			return m
		}
	}
	return p[start]
}

// InPool reports whether mac belongs to the course + voice pool.
func InPool(idx, voice int, mac string) bool {
	mac = strings.ToLower(strings.TrimSpace(mac))
	for _, m := range Pool(idx, voice) {
		if m == mac {
			return true
		}
	}
	return false
}

// Selection is the lesson the robot is on. Book is 0-based, Unit 1-based.
type Selection struct {
	Idx   int
	Voice int
	Book  int
	Unit  int
}

// Normalize clamps a selection into the catalog (unknown course → default).
func Normalize(s Selection) Selection {
	c, ok := Get(s.Idx)
	if !ok {
		c, _ = Get(DefaultIdx)
		s.Idx = c.Idx
	}
	s.Voice = normVoice(s.Voice)
	if s.Book < 0 || s.Book >= len(c.Books) {
		s.Book = 0
	}
	n := len(c.Books[s.Book].Units)
	if s.Unit < 1 {
		s.Unit = 1
	}
	if s.Unit > n {
		s.Unit = n
	}
	return s
}

// UnitName returns the unit title for a (normalized) selection.
func UnitName(s Selection) string {
	s = Normalize(s)
	c, _ := Get(s.Idx)
	return c.Books[s.Book].Units[s.Unit-1]
}

// BookName returns the book title for a (normalized) selection.
func BookName(s Selection) string {
	s = Normalize(s)
	c, _ := Get(s.Idx)
	return c.Books[s.Book].Name
}

// Shift moves to the next (delta>0) or previous unit; past the end of a book
// it continues in the next/previous book. clamped is true at the very ends.
func Shift(s Selection, delta int) (out Selection, clamped bool) {
	s = Normalize(s)
	c, _ := Get(s.Idx)
	switch {
	case delta > 0:
		if s.Unit < len(c.Books[s.Book].Units) {
			s.Unit++
		} else if s.Book < len(c.Books)-1 {
			s.Book++
			s.Unit = 1
		} else {
			clamped = true
		}
	case delta < 0:
		if s.Unit > 1 {
			s.Unit--
		} else if s.Book > 0 {
			s.Book--
			s.Unit = len(c.Books[s.Book].Units)
		} else {
			clamped = true
		}
	}
	return s, clamped
}

// ShiftMessage is the short Vietnamese reply TTS reads after Shift.
func ShiftMessage(before, after Selection, delta int, clamped bool) string {
	unit := UnitName(after)
	book := BookName(after)
	c, _ := Get(after.Idx)
	suffix := ""
	if len(c.Books) > 1 {
		suffix = " (" + book + ")"
	}
	switch {
	case clamped && delta > 0:
		return "Đã ở unit cuối: " + unit + suffix
	case clamped:
		return "Đã ở unit đầu: " + unit + suffix
	case before.Book != after.Book:
		dir := "tiếp theo"
		if delta < 0 {
			dir = "trước"
		}
		return "Đã chuyển sách → " + book + ", unit " + dir + ": " + unit
	case delta > 0:
		return "Đã chọn unit tiếp theo: " + unit
	default:
		return "Đã chọn unit trước: " + unit
	}
}

// ParseDirection maps next/prev (and Vietnamese) to +1/-1; 0 if unknown.
func ParseDirection(d string) int {
	d = strings.ToLower(strings.TrimSpace(d))
	switch {
	case d == "next" || d == "+" || d == "1" || strings.Contains(d, "tiếp") || strings.Contains(d, "tiep"):
		return 1
	case d == "prev" || d == "previous" || d == "-" || d == "-1" ||
		strings.Contains(d, "trước") || strings.Contains(d, "truoc"):
		return -1
	}
	return 0
}
