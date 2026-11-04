package superagent

import "testing"

func TestParsePlanClean(t *testing.T) {
	p, err := ParsePlan(`{"steps": ["do a", "check b"], "done_criteria": "b holds"}`)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(p.Steps) != 2 || p.Steps[0] != "do a" || p.DoneCriteria != "b holds" {
		t.Fatalf("got %+v", p)
	}
}

func TestParsePlanFenced(t *testing.T) {
	reply := "Here is the plan:\n```json\n{\"steps\": [\"a\", \"b\"], \"done_criteria\": \"ok\"}\n```\nHope that helps."
	p, err := ParsePlan(reply)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("got %+v", p)
	}
}

func TestParsePlanGarbage(t *testing.T) {
	if _, err := ParsePlan("I cannot make a plan right now."); err == nil {
		t.Fatal("want error for non-JSON reply")
	}
	if _, err := ParsePlan(`{"steps": []}`); err == nil {
		t.Fatal("want error for empty steps")
	}
	steps := make([]string, 9)
	for i := range steps {
		steps[i] = `s`
	}
	if _, err := ParsePlan(`{"steps": [` + joinQuoted(steps) + `]}`); err == nil {
		t.Fatal("want error for more than 8 steps")
	}
}

func TestParsePlanDefaultDoneCriteria(t *testing.T) {
	p, err := ParsePlan(`{"steps": ["a"]}`)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if p.DoneCriteria != "all steps completed" {
		t.Fatalf("done criteria = %q", p.DoneCriteria)
	}
}

func joinQuoted(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += `"` + s + `"`
	}
	return out
}
