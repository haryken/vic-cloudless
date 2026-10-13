package xiaozhi

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/course"
)

// IdentityCourse is the English-teaching preset: the MAC comes from the pool
// of the selected course + voice, rotated on every vic-cloud start like vi_pool.
const IdentityCourse = "course"

// IsCourse reports whether cfg uses an English-teaching course identity.
func IsCourse(cfg Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.IdentityMode), IdentityCourse)
}

func courseKey(idx int) string { return strconv.Itoa(idx) }

func courseUnitKey(idx, book int) string { return strconv.Itoa(idx) + "." + strconv.Itoa(book) }

// CourseSelection returns the lesson stored in cfg (normalized).
func CourseSelection(cfg Config) course.Selection {
	idx := cfg.CourseIdx
	s := course.Selection{
		Idx:   idx,
		Voice: cfg.CourseVoice[courseKey(idx)],
		Book:  cfg.CourseBook[courseKey(idx)],
	}
	s.Unit = cfg.CourseUnit[courseUnitKey(idx, s.Book)]
	return course.Normalize(s)
}

func storeCourseLesson(cfg *Config, s course.Selection) {
	if cfg.CourseBook == nil {
		cfg.CourseBook = map[string]int{}
	}
	if cfg.CourseUnit == nil {
		cfg.CourseUnit = map[string]int{}
	}
	cfg.CourseBook[courseKey(s.Idx)] = s.Book
	cfg.CourseUnit[courseUnitKey(s.Idx, s.Book)] = s.Unit
}

// RotateCourseIdentity picks another MAC from the course + voice pool and a
// new client UUID (boot-time, same as RotateViPoolIdentity).
func RotateCourseIdentity(cfg *Config) {
	s := CourseSelection(*cfg)
	cfg.IdentityMode = IdentityCourse
	cfg.CourseIdx = s.Idx
	cfg.OTABaseURL = TenclassOTA
	cfg.Endpoint = TenclassWSS
	cfg.AutoApplyOTAWebsocket = true
	cfg.TTSMode = "xiaozhi"
	cfg.DeviceID = course.PickMAC(s.Idx, s.Voice, cfg.DeviceID)
	cfg.ClientID = genUUIDv4()
	cfg.Token = ""
}

// courseToolsEnabled: the course tools are listed only for course identities.
func courseToolsEnabled(cfg Config) bool { return IsCourse(cfg) }

// Tool names and descriptions match xiaozhi-esp32 otto-robot (otto_controller.cc)
// because the teaching agents bound to these MAC pools were written against
// them. self.mini.* / self.vector.* prefixes and the older shift_unit are
// still accepted on call.
const (
	toolStudentInfo = "self.otto.get_student_info"
	toolNextUnit    = "self.otto.next_unit"
	toolPrevUnit    = "self.otto.prev_unit"
)

func courseToolDefs() []map[string]interface{} {
	return []map[string]interface{}{
		toolDef(toolStudentInfo,
			"Lấy tên học viên và unit đang học. Dùng khi hỏi 'tao là ai', 'đang học unit gì'.",
			nil, nil),
		toolDef(toolNextUnit,
			"Chuyển sang UNIT TIẾP THEO trong sách/khóa hiện tại và LƯU vào robot (Self-Control web cũng thấy). "+
				"Dùng khi người dùng nói: 'unit tiếp theo', 'học unit sau', 'next unit', 'chuyển unit kế'. "+
				"Trả về unit_name mới — hãy dạy tiếp unit đó.",
			nil, nil),
		toolDef(toolPrevUnit,
			"Chuyển sang UNIT TRƯỚC ĐÓ trong sách/khóa hiện tại và LƯU vào robot (Self-Control web cũng thấy). "+
				"Dùng khi người dùng nói: 'unit trước', 'unit trước đó', 'previous unit', 'lui unit'. "+
				"Trả về unit_name mới — hãy dạy tiếp unit đó.",
			nil, nil),
	}
}

// courseToolKind maps a called tool name (any accepted prefix) to its action.
func courseToolKind(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, p := range []string{"self.otto.", "self.mini.", "self.vector."} {
		if !strings.HasPrefix(n, p) {
			continue
		}
		switch strings.TrimPrefix(n, p) {
		case "get_student_info":
			return "info"
		case "shift_unit":
			return "shift"
		case "next_unit":
			return "next"
		case "prev_unit":
			return "prev"
		}
	}
	return ""
}

func studentInfoJSON(cfg Config) string {
	s := CourseSelection(cfg)
	c, _ := course.Get(s.Idx)
	info := map[string]interface{}{
		"student_name": cfg.StudentName,
		"course":       c.ID,
		"course_name":  c.Name,
		"course_idx":   s.Idx,
		"voice":        c.Voices[s.Voice].Label,
		"device_id":    cfg.DeviceID,
		"sub_idx":      s.Book,
		"unit_index":   s.Unit,
		"units":        strconv.Itoa(s.Unit),
		"unit_names":   course.UnitName(s),
	}
	if len(c.Books) > 1 {
		info["sub_name"] = course.BookName(s)
	}
	b, _ := json.Marshal(info)
	return string(b)
}

// handleCourseTool runs a course tool; returns (text, isError).
func handleCourseTool(kind string, args map[string]interface{}) (string, bool) {
	cfg := LoadConfig()
	if !IsCourse(cfg) {
		return "Robot chưa chọn khóa học tiếng Anh trên trang cấu hình.", true
	}
	if kind == "info" {
		log.Println("[Xiaozhi][Course] get_student_info")
		return studentInfoJSON(cfg), false
	}
	delta := 1
	switch kind {
	case "prev":
		delta = -1
	case "shift":
		dir, _ := args["direction"].(string)
		if delta = course.ParseDirection(dir); delta == 0 {
			return "direction phải là next hoặc prev", true
		}
	}
	before := CourseSelection(cfg)
	after, clamped := course.Shift(before, delta)
	storeCourseLesson(&cfg, after)
	if err := SaveConfig(cfg); err != nil {
		log.Println("[Xiaozhi][Course] save after shift:", err)
		return "Không lưu được unit mới.", true
	}
	msg := course.ShiftMessage(before, after, delta, clamped)
	log.Printf("[Xiaozhi][Course] shift %+d → book %d unit %d: %s", delta, after.Book, after.Unit, msg)
	b, _ := json.Marshal(map[string]interface{}{
		"success":    true,
		"message":    msg,
		"course_idx": after.Idx,
		"sub_idx":    after.Book,
		"unit_index": after.Unit,
		"unit_idx":   after.Unit,
		"unit_name":  course.UnitName(after),
		"clamped":    clamped,
	})
	return string(b), false
}
