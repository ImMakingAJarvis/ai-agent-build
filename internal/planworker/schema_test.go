package planworker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTaskSchema compiles the real schema file the pipeline loads at
// startup, so the tests pin the same contract the worker enforces.
func testTaskSchema(t *testing.T) *TaskSchema {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "prompts", SchemaFile))
	if err != nil {
		t.Fatalf("read %s: %v", SchemaFile, err)
	}
	s, err := NewTaskSchema(data)
	if err != nil {
		t.Fatalf("NewTaskSchema: %v", err)
	}
	return s
}

// TestSchemaValidation is the B3 acceptance test: a sample decompose
// output validates against the task schema and decodes to the Go types.
func TestSchemaValidation(t *testing.T) {
	s := testTaskSchema(t)
	data, err := os.ReadFile(filepath.Join("testdata", "tasks-valid.json"))
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}
	tl, err := s.Validate(data)
	if err != nil {
		t.Fatalf("Validate(sample): %v", err)
	}
	if tl.Epic != "wi-1" || len(tl.Tasks) != 2 {
		t.Fatalf("decoded = %+v", tl)
	}
	first := tl.Tasks[0]
	if first.ID != "add-schema" || first.Size != "M" ||
		first.ExecutorModel != "qwen3.8-27b" ||
		first.AcceptanceCheck != "go test ./internal/planworker/ -run TestSchemaValidation" ||
		len(first.Files) != 2 {
		t.Fatalf("task[0] = %+v", first)
	}
	second := tl.Tasks[1]
	if len(second.Dependencies) != 1 || second.Dependencies[0] != "add-schema" {
		t.Fatalf("task[1].dependencies = %v", second.Dependencies)
	}
}

const oneTask = `{"id":"a","title":"t","description":"d","acceptance_check":"go test ./...","size":"S","executor_model":"m"}`

// TestSchemaRejectsInvalid pins every rule the schema enforces: the sample
// must pass, and each of these replies from a decompose model must be
// rejected with a detail that names the problem.
func TestSchemaRejectsInvalid(t *testing.T) {
	s := testTaskSchema(t)
	cases := []struct {
		name, in, want string
	}{
		{"not json", `nope`, "parse task list"},
		{"empty tasks", `{"epic":"wi-1","tasks":[]}`, "tasks"},
		{"missing epic", `{"tasks":[` + oneTask + `]}`, "epic"},
		{"missing required field",
			`{"epic":"wi-1","tasks":[{"id":"a","title":"t","description":"d","size":"S","executor_model":"m"}]}`,
			"acceptance_check"},
		{"bad size",
			`{"epic":"wi-1","tasks":[{"id":"a","title":"t","description":"d","acceptance_check":"x","size":"L","executor_model":"m"}]}`,
			"size"},
		{"bad id pattern",
			`{"epic":"wi-1","tasks":[{"id":"Add Schema","title":"t","description":"d","acceptance_check":"x","size":"S","executor_model":"m"}]}`,
			"id"},
		{"extra property", `{"epic":"wi-1","tasks":[` + oneTask + `],"bogus":true}`, "bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Validate([]byte(tc.in))
			if err == nil {
				t.Fatalf("Validate(%q) = nil error", tc.in)
			}
			if got := ValidationErrorDetail(err); !strings.Contains(got, tc.want) {
				t.Fatalf("detail = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// TestSchemaValidationErrorDetail checks the retry-prompt message carries
// the location of the offending value, not just "schema validation failed".
func TestSchemaValidationErrorDetail(t *testing.T) {
	s := testTaskSchema(t)
	in := `{"epic":"wi-1","tasks":[{"id":"a","title":"t","description":"d","acceptance_check":"x","size":"L","executor_model":"m"}]}`
	_, err := s.Validate([]byte(in))
	if err == nil {
		t.Fatal("Validate: expected error")
	}
	got := ValidationErrorDetail(err)
	for _, want := range []string{"/tasks/0/size", "one of 'S', 'M'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail = %q, want it to contain %q", got, want)
		}
	}
}

// TestSchemaConsistency covers the cross-task rules the schema cannot
// express: unique ids and dependencies that name a task in the list.
func TestSchemaConsistency(t *testing.T) {
	s := testTaskSchema(t)
	const other = `{"id":"b","title":"t2","description":"d2","acceptance_check":"go test ./...","size":"S","executor_model":"m"}`
	cases := []struct {
		name, in, want string
	}{
		{"duplicate id",
			`{"epic":"wi-1","tasks":[` + oneTask + `,` + oneTask + `]}`,
			"duplicate task id"},
		{"self dependency",
			`{"epic":"wi-1","tasks":[` +
				`{"id":"a","title":"t","description":"d","acceptance_check":"x","size":"S","executor_model":"m","dependencies":["a"]}` +
				`]}`,
			"depends on itself"},
		{"unknown dependency",
			`{"epic":"wi-1","tasks":[` + other +
				`,` +
				`{"id":"a","title":"t","description":"d","acceptance_check":"x","size":"S","executor_model":"m","dependencies":["nope"]}` +
				`]}`,
			"unknown task"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Validate([]byte(tc.in))
			if err == nil {
				t.Fatalf("Validate(%q) = nil error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want %q", err, tc.want)
			}
		})
	}
}
