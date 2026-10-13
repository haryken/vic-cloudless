package xiaozhi

import (
	"encoding/json"
	"testing"
)

// The teaching agents were written against xiaozhi-esp32 otto-robot: the
// advertised names must stay exactly these.
func TestCourseToolNamesMatchOtto(t *testing.T) {
	var names []string
	for _, d := range courseToolDefs() {
		names = append(names, d["name"].(string))
	}
	want := []string{"self.otto.get_student_info", "self.otto.next_unit", "self.otto.prev_unit"}
	if len(names) != len(want) {
		t.Fatalf("tools %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tools %v, want %v", names, want)
		}
	}
	for name, kind := range map[string]string{
		"self.otto.get_student_info": "info",
		"self.otto.next_unit":        "next",
		"self.otto.prev_unit":        "prev",
		"self.otto.shift_unit":       "shift",
		"self.vector.next_unit":      "next",
	} {
		if got := courseToolKind(name); got != kind {
			t.Fatalf("%s → %q, want %q", name, got, kind)
		}
	}
}

func TestStudentInfoJSONOttoFields(t *testing.T) {
	cfg := Config{IdentityMode: IdentityCourse, StudentName: "Linh", CourseIdx: 1}
	var info map[string]interface{}
	if err := json.Unmarshal([]byte(studentInfoJSON(cfg)), &info); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"student_name", "course_idx", "unit_index", "unit_names"} {
		if _, ok := info[k]; !ok {
			t.Fatalf("missing %s in %v", k, info)
		}
	}
	if info["student_name"] != "Linh" {
		t.Fatalf("student_name %v", info["student_name"])
	}
}
