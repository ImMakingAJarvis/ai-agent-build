package planworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// enPrinter renders jsonschema error messages in English.
var enPrinter = message.NewPrinter(language.English)

// SchemaFile is the name of the task JSON schema inside the prompts dir.
// It is the machine contract for the decompose step's output (B3).
const SchemaFile = "task.schema.json"

// schemaURI is the internal location the schema is compiled under; it is
// not a real path, only a key for the compiler.
const schemaURI = "urn:ai-agent-build:task.schema.json"

// TaskList is the decompose step's output: an epic's plan broken into
// buildable tasks (docs/work-board/PLAN.md, "Planning pipeline", step 2).
type TaskList struct {
	// Epic is the Plane work-item ID of the epic this list belongs to.
	Epic string `json:"epic"`
	// Tasks is the list, in the order it should be attempted.
	Tasks []Task `json:"tasks"`
}

// Task is one unit of work: one fresh-context queue job for an executor
// model. Its fields are the exact shape the task schema requires.
type Task struct {
	// ID is a short kebab-case id, unique within the list.
	ID string `json:"id"`
	// Title is one line: what the task delivers.
	Title string `json:"title"`
	// Description is what to build and how, specific enough for a model
	// with no other context.
	Description string `json:"description"`
	// Files are the repo paths the task is likely to touch.
	Files []string `json:"files,omitempty"`
	// Dependencies are ids of tasks that must be Done first.
	Dependencies []string `json:"dependencies,omitempty"`
	// AcceptanceCheck is a test name or command that must pass for the
	// task to be Done.
	AcceptanceCheck string `json:"acceptance_check"`
	// Size is "S" or "M" only.
	Size string `json:"size"`
	// ExecutorModel is the registry id of the suggested executor.
	ExecutorModel string `json:"executor_model"`
}

// TaskSchema is a compiled task JSON schema plus the cross-field rules it
// cannot express. Build one with NewTaskSchema and reuse it to validate
// every decompose reply.
type TaskSchema struct {
	compiled *jsonschema.Schema
}

// NewTaskSchema compiles the task schema from data (the bytes of
// prompts/task.schema.json). Compilation fails on a malformed schema, so a
// Pipeline cannot start with a broken contract.
func NewTaskSchema(data []byte) (*TaskSchema, error) {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURI, doc); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	sch, err := c.Compile(schemaURI)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return &TaskSchema{compiled: sch}, nil
}

// Validate parses data, checks it against the schema, and applies the
// cross-field rules the schema cannot express (task ids are unique, and
// every dependency names a task in the list). It returns the decoded list.
// A schema failure is returned as the *jsonschema.ValidationError so a
// caller can feed its message back to the model.
func (s *TaskSchema) Validate(data []byte) (TaskList, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return TaskList{}, fmt.Errorf("parse task list: %w", err)
	}
	if err := s.compiled.Validate(v); err != nil {
		return TaskList{}, err
	}
	var tl TaskList
	if err := json.Unmarshal(data, &tl); err != nil {
		return TaskList{}, fmt.Errorf("decode task list: %w", err)
	}
	if err := tl.checkConsistency(); err != nil {
		return TaskList{}, err
	}
	return tl, nil
}

// ValidationErrorDetail renders a schema validation failure as one
// human-readable line per violated rule, each carrying the location of the
// offending value, e.g.
//
//	at /tasks/0/size: "L" is not one of [S, M]
//
// It is what the decompose retry prompt feeds back to the model (the bare
// *jsonschema.ValidationError only names the schema URI) and what tests
// assert on. A non-schema error is returned with its Error() text.
func ValidationErrorDetail(err error) string {
	var v *jsonschema.ValidationError
	if !errors.As(err, &v) {
		return err.Error()
	}
	var lines []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if e.ErrorKind != nil {
			lines = append(lines,
				"at "+instancePointer(e.InstanceLocation)+": "+
					e.ErrorKind.LocalizedString(enPrinter))
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(v)
	if len(lines) == 0 {
		return v.Error()
	}
	return strings.Join(lines, "\n")
}

// instancePointer renders a jsonschema instance location as a JSON pointer,
// escaping the two reserved characters per RFC 6901.
func instancePointer(loc []string) string {
	if len(loc) == 0 {
		return "$"
	}
	var b strings.Builder
	for _, t := range loc {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// checkConsistency enforces the rules that live across tasks and so cannot
// be expressed in the per-task schema: ids are unique, and every
// dependency references a task that exists and is not the task itself.
func (tl TaskList) checkConsistency() error {
	ids := make(map[string]bool, len(tl.Tasks))
	for _, t := range tl.Tasks {
		if ids[t.ID] {
			return fmt.Errorf("duplicate task id %q", t.ID)
		}
		ids[t.ID] = true
	}
	for _, t := range tl.Tasks {
		for _, d := range t.Dependencies {
			if d == t.ID {
				return fmt.Errorf("task %q depends on itself", t.ID)
			}
			if !ids[d] {
				return fmt.Errorf("task %q depends on unknown task %q", t.ID, d)
			}
		}
	}
	return nil
}
